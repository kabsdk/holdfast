// Package agent is holdfast-agent: it runs its cached config on its own, whether the server is
// reachable or not (decision 006). The server configures it; it never needs the server to back up.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/schedule"
)

// Version is set at build time.
var Version = "dev"

// jobState is what the scheduler remembers per job, across restarts.
type jobState struct {
	ScheduleSHA string    `json:"schedule_sha"`
	LastSlot    time.Time `json:"last_slot"`    // newest nominal slot handled: run, caught up or skipped
	NextSlot    time.Time `json:"next_slot"`    // the planned slot; zero: run as soon as possible
	NextDelayS  int       `json:"next_delay_s"` // spread for NextSlot, drawn once and kept over restarts
	LastAttempt time.Time `json:"last_attempt"`
	LastStatus  string    `json:"last_status"`
}

type runMarker struct {
	Kind      string    `json:"kind"`
	JobID     string    `json:"job_id,omitempty"`
	CommandID string    `json:"command_id,omitempty"`
	Started   time.Time `json:"started"`
	Revision  int64     `json:"revision"`
}

// Agent is one running agent.
type Agent struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	mu        sync.Mutex
	token     string
	hc        *proto.HostConfig
	credVer   int
	sched     map[string]*jobState
	toAck     []string
	done      map[string]time.Time // command IDs already run, so a re-sent command runs once
	running   *proto.Running
	factsOnce sync.Once
	base      proto.Facts // facts that do not change while the agent runs

	wake     chan struct{}
	flushNow chan struct{}
}

// New prepares an agent from its configuration and state directory.
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	cfg.Defaults()
	if log == nil {
		log = slog.Default()
	}
	for _, d := range []string{cfg.StateDir, filepath.Join(cfg.StateDir, "spool"), filepath.Join(cfg.StateDir, "commands"), filepath.Join(cfg.StateDir, "cache"), cfg.CredentialsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	client, err := httpClient(cfg.CASHA256)
	if err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, client: client, log: log, now: time.Now, sched: map[string]*jobState{}, done: map[string]time.Time{},
		wake: make(chan struct{}, 1), flushNow: make(chan struct{}, 1)}
	a.load()
	return a, nil
}

// httpClient pins the server when a fingerprint is given: some certificate in the chain the server
// presents must have that sha256.
func httpClient(pin string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if pin != "" {
		pin = strings.ToLower(strings.ReplaceAll(pin, ":", ""))
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // replaced by the pin check below
			VerifyConnection: func(cs tls.ConnectionState) error {
				for _, c := range cs.PeerCertificates {
					sum := sha256.Sum256(c.Raw)
					if hex.EncodeToString(sum[:]) == pin {
						return nil
					}
				}
				return errors.New("the server's certificate chain does not match the pinned fingerprint")
			},
		}
	}
	return &http.Client{Transport: tr}, nil
}

func (a *Agent) path(name string) string { return filepath.Join(a.cfg.StateDir, name) }

func (a *Agent) load() {
	if b, err := os.ReadFile(a.path("token")); err == nil {
		a.token = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(a.path("config.json")); err == nil {
		var hc proto.HostConfig
		// A cached config that does not match its own hash was edited by hand: report it as drift by
		// treating it as absent, so the server sends the right one again.
		if json.Unmarshal(b, &hc) == nil && hc.ComputeSHA256() == hc.SHA256 {
			a.hc = &hc
		} else {
			a.log.Warn("cached config does not match its hash; ignoring it until the server sends it again")
		}
	}
	if b, err := os.ReadFile(a.path("credentials.version")); err == nil {
		fmt.Sscan(string(b), &a.credVer)
	}
	if b, err := os.ReadFile(a.path("schedule.json")); err == nil {
		_ = json.Unmarshal(b, &a.sched)
	}
	if b, err := os.ReadFile(a.path("commands-done.json")); err == nil {
		_ = json.Unmarshal(b, &a.done)
	}
}

// writeFileAtomic writes through a temporary file and a rename, so a crash never leaves half a file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *Agent) saveSchedule() {
	b, _ := json.MarshalIndent(a.sched, "", "  ")
	if err := writeFileAtomic(a.path("schedule.json"), b, 0o600); err != nil {
		a.log.Error("saving schedule state", "err", err)
	}
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

/* ---------------- enrollment ---------------- */

// Enroll redeems a code and stores the token. It is run once, by `holdfast-agent enroll`.
func Enroll(ctx context.Context, cfg Config, code string, log *slog.Logger) error {
	a, err := New(cfg, log)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(proto.EnrollRequest{FQDN: cfg.FQDN, Code: code, Facts: a.facts(ctx)})
	var resp proto.EnrollResponse
	if err := a.call(ctx, "/enroll", "", body, &resp); err != nil {
		return err
	}
	if err := writeFileAtomic(a.path("token"), []byte(resp.Token), 0o600); err != nil {
		return err
	}
	log.Info("enrolled", "fqdn", cfg.FQDN)
	return nil
}

func (a *Agent) call(ctx context.Context, path, query string, body []byte, out any) error {
	url := strings.TrimSuffix(a.cfg.Server, "/") + proto.Base + path
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.mu.Lock()
	tok := a.token
	a.mu.Unlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e proto.ErrorResponse
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

/* ---------------- facts ---------------- */

// facts returns the host facts. The ones that do not change are read once.
func (a *Agent) facts(ctx context.Context) proto.Facts {
	a.factsOnce.Do(func() { a.base = a.readFacts(ctx) })
	f := a.base
	if f.ResticVersion == "" { // restic may be installed after the agent started
		if v, err := (restic.Runner{Bin: a.cfg.ResticBin}).Version(ctx); err == nil {
			a.base.ResticVersion, f.ResticVersion = v, v
		}
	}
	f.Now = a.now().UTC()
	return f
}

func (a *Agent) readFacts(ctx context.Context) proto.Facts {
	f := proto.Facts{AgentVersion: Version}
	if v, err := (restic.Runner{Bin: a.cfg.ResticBin}).Version(ctx); err == nil {
		f.ResticVersion = v
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.Trim(v, `"`)
			switch k {
			case "ID":
				f.OSID = v
			case "VERSION_ID":
				f.OSVersion = v
			}
		}
	}
	if out, err := exec.CommandContext(ctx, "hostname", "-f").Output(); err == nil {
		f.HostnameFQDN = strings.TrimSpace(string(out))
	}
	f.Timezone = time.Local.String()
	if runtime.GOOS != "linux" {
		f.OSID = runtime.GOOS
	}
	return f
}

/* ---------------- run loops ---------------- */

// Run starts the agent and blocks until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if a.token == "" {
		return errors.New("not enrolled: run holdfast-agent enroll first")
	}
	a.recoverInterrupted(ctx)
	// Pick up changes made while the agent was down before running anything; with the server down,
	// carry on from the cached config.
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := a.pollOnce(sctx, 0); err != nil {
		a.log.Warn("startup sync failed; running from the cached config", "err", err)
	}
	cancel()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.pollLoop(ctx) }()
	go func() { defer wg.Done(); a.flushLoop(ctx) }()
	go func() { defer wg.Done(); a.workLoop(ctx) }()
	wg.Wait()
	return nil
}

func (a *Agent) pollLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := a.pollOnce(ctx, a.cfg.PollTimeout); err != nil {
			if ctx.Err() == nil {
				a.log.Warn("poll failed; scheduled backups continue from the cached config", "err", err)
			}
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
	}
}

func (a *Agent) pollOnce(ctx context.Context, wait time.Duration) error {
	a.mu.Lock()
	req := proto.PollRequest{CredentialsVersion: a.credVer, AckedCommands: append([]string(nil), a.toAck...), Running: a.running}
	if a.hc != nil {
		req.Applied = proto.Applied{Revision: a.hc.Revision, SHA256: a.hc.SHA256}
	}
	a.mu.Unlock()
	req.Facts = a.facts(ctx)
	body, _ := json.Marshal(req)
	pctx, cancel := context.WithTimeout(ctx, wait+15*time.Second)
	defer cancel()
	var resp proto.PollResponse
	if err := a.call(pctx, "/poll", "wait="+wait.String(), body, &resp); err != nil {
		return err
	}
	a.mu.Lock()
	a.toAck = a.toAck[len(req.AckedCommands):] // the server has these now
	a.mu.Unlock()
	if resp.Credentials != nil {
		if err := a.applyCredentials(*resp.Credentials); err != nil {
			return err
		}
	}
	if resp.Config != nil {
		if err := a.applyConfig(*resp.Config); err != nil {
			return err
		}
	}
	for _, c := range resp.Commands {
		a.queueCommand(c)
	}
	return nil
}

func (a *Agent) applyCredentials(c proto.Credentials) error {
	if err := writeFileAtomic(filepath.Join(a.cfg.CredentialsDir, "repo.pass"), []byte(c.RepoPassword), 0o600); err != nil {
		return err
	}
	env := "AWS_ACCESS_KEY_ID=" + c.S3AccessKeyID + "\nAWS_SECRET_ACCESS_KEY=" + c.S3SecretAccessKey + "\n"
	if err := writeFileAtomic(filepath.Join(a.cfg.CredentialsDir, "s3.env"), []byte(env), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(a.path("credentials.version"), []byte(fmt.Sprint(c.Version)), 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	a.credVer = c.Version
	a.mu.Unlock()
	a.log.Info("applied credentials", "version", c.Version)
	return nil
}

func (a *Agent) applyConfig(hc proto.HostConfig) error {
	if hc.ComputeSHA256() != hc.SHA256 {
		return errors.New("the server sent a config that does not match its own hash")
	}
	b, _ := json.MarshalIndent(hc, "", "  ")
	if err := writeFileAtomic(a.path("config.json"), b, 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	a.hc = &hc
	a.mu.Unlock()
	a.log.Info("applied config", "revision", hc.Revision, "jobs", len(hc.Jobs))
	nudge(a.wake)
	return nil
}

func (a *Agent) queueCommand(c proto.Command) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ran := a.done[c.ID]; ran {
		a.toAck = append(a.toAck, c.ID)
		return
	}
	b, _ := json.Marshal(c)
	if err := writeFileAtomic(filepath.Join(a.path("commands"), c.ID+".json"), b, 0o600); err != nil {
		a.log.Error("storing command", "err", err)
		return
	}
	a.toAck = append(a.toAck, c.ID) // acknowledged on the next poll, now that it is on disk
	nudge(a.wake)
}

/* ---------------- spool ---------------- */

// spool keeps an event on disk until the server acknowledges it.
func (a *Agent) spool(ev proto.Event) {
	if ev.ID == "" {
		ev.ID = newID()
	}
	b, _ := json.Marshal(ev)
	name := fmt.Sprintf("%d-%s.json", a.now().UnixNano(), ev.ID)
	if err := writeFileAtomic(filepath.Join(a.path("spool"), name), b, 0o600); err != nil {
		a.log.Error("spooling event", "err", err)
	}
	a.log.Info("run finished", "kind", ev.Kind, "job", ev.JobID, "status", ev.Status, "snapshot", ev.SnapshotID, "error", ev.Error)
	nudge(a.flushNow)
}

func (a *Agent) flushLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.flushNow:
		}
		if err := a.flush(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("delivering events failed; they stay in the spool", "err", err)
		}
	}
}

func (a *Agent) flush(ctx context.Context) error {
	dir := a.path("spool")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	var events []proto.Event
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var ev proto.Event
		if json.Unmarshal(b, &ev) != nil {
			continue
		}
		names, events = append(names, e.Name()), append(events, ev)
		if len(events) == 200 {
			break
		}
	}
	if len(events) == 0 {
		return nil
	}
	body, _ := json.Marshal(proto.EventsRequest{Events: events})
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var resp proto.EventsResponse
	if err := a.call(rctx, "/events", "", body, &resp); err != nil {
		return err
	}
	acked := map[string]bool{}
	for _, id := range resp.Acked {
		acked[id] = true
	}
	for i, ev := range events {
		if acked[ev.ID] {
			_ = os.Remove(filepath.Join(dir, names[i]))
		}
	}
	return nil
}

/* ---------------- scheduling and runs ---------------- */

func scheduleSHA(s schedule.Schedule) string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func randomDelay(minutes int) int {
	if minutes <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(minutes*60)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

// plan makes sure a job's next slot and its spread are chosen. Caller holds a.mu.
func (a *Agent) plan(j proto.JobConfig, now time.Time) *jobState {
	st := a.sched[j.ID]
	sha := scheduleSHA(j.Schedule)
	if st == nil {
		// A job that never ran runs at once (first backup right after enrollment, plan-m1 Q7).
		st = &jobState{ScheduleSHA: sha}
		a.sched[j.ID] = st
		return st
	}
	if st.ScheduleSHA != sha {
		st.ScheduleSHA, st.NextSlot = sha, time.Time{} // re-plan from the last handled slot
	}
	if st.NextSlot.IsZero() && !st.LastSlot.IsZero() {
		loc, err := j.Schedule.Location(time.Local)
		if err != nil {
			loc = time.Local
		}
		if next, ok := j.Schedule.Next(st.LastSlot, loc); ok {
			st.NextSlot = next
			st.NextDelayS = 0
			if j.Schedule.Spread.Enabled {
				st.NextDelayS = randomDelay(j.Schedule.Spread.Minutes)
			}
		}
	}
	return st
}

// due returns the most overdue job, if any. A slot missed while the agent was down is due at once
// and runs once, however many slots were missed (plan-m1 Q7).
func (a *Agent) due(now time.Time) (proto.JobConfig, *jobState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hc == nil {
		return proto.JobConfig{}, nil, false
	}
	type cand struct {
		j  proto.JobConfig
		st *jobState
		at time.Time
	}
	var cands []cand
	for _, j := range a.hc.Jobs {
		st := a.plan(j, now)
		if st.LastSlot.IsZero() {
			// Never completed a slot: the first backup, or a first backup that was interrupted.
			cands = append(cands, cand{j, st, time.Time{}})
			continue
		}
		if st.NextSlot.IsZero() {
			continue
		}
		at := st.NextSlot.Add(time.Duration(st.NextDelayS) * time.Second)
		if !now.Before(at) {
			cands = append(cands, cand{j, st, at})
		}
	}
	a.saveSchedule()
	if len(cands) == 0 {
		return proto.JobConfig{}, nil, false
	}
	sort.Slice(cands, func(i, k int) bool { return cands[i].at.Before(cands[k].at) })
	return cands[0].j, cands[0].st, true
}

func (a *Agent) workLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.wake:
		}
		// One restic process per host (plan-m1 Q7): commands first, then due jobs, one at a time.
		for ctx.Err() == nil {
			if c, ok := a.nextCommand(); ok {
				a.execCommand(ctx, c)
				continue
			}
			j, st, ok := a.due(a.now())
			if !ok {
				break
			}
			a.runScheduled(ctx, j, st)
		}
	}
}

func (a *Agent) nextCommand() (proto.Command, bool) {
	entries, err := os.ReadDir(a.path("commands"))
	if err != nil {
		return proto.Command{}, false
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(a.path("commands"), e.Name())
		b, err := os.ReadFile(p)
		var c proto.Command
		if err != nil || json.Unmarshal(b, &c) != nil {
			_ = os.Remove(p)
			continue
		}
		return c, true
	}
	return proto.Command{}, false
}

func (a *Agent) finishCommand(id string) {
	_ = os.Remove(filepath.Join(a.path("commands"), id+".json"))
	a.mu.Lock()
	a.done[id] = a.now()
	for k, t := range a.done { // remember a week of command IDs
		if a.now().Sub(t) > 7*24*time.Hour {
			delete(a.done, k)
		}
	}
	b, _ := json.Marshal(a.done)
	a.mu.Unlock()
	_ = writeFileAtomic(a.path("commands-done.json"), b, 0o600)
}

func (a *Agent) execCommand(ctx context.Context, c proto.Command) {
	defer a.finishCommand(c.ID)
	switch c.Kind {
	case proto.CommandBackupNow:
		a.mu.Lock()
		var job *proto.JobConfig
		if a.hc != nil {
			for i := range a.hc.Jobs {
				if a.hc.Jobs[i].ID == c.JobID {
					job = &a.hc.Jobs[i]
				}
			}
		}
		a.mu.Unlock()
		if job == nil {
			now := a.now().UTC()
			a.spool(proto.Event{Kind: proto.KindBackup, JobID: c.JobID, CommandID: c.ID, Status: proto.StatusFailed, Started: now, Finished: now, Error: "unknown job"})
			return
		}
		a.runBackup(ctx, *job, c.ID, nil, 0)
	default:
		now := a.now().UTC()
		a.spool(proto.Event{Kind: c.Kind, CommandID: c.ID, Status: proto.StatusFailed, Started: now, Finished: now, Error: "unknown command kind"})
	}
}

// runScheduled runs a due job and then advances its schedule. Slots of this job that came due
// while it ran are skipped and reported (plan-m1 Q7).
func (a *Agent) runScheduled(ctx context.Context, j proto.JobConfig, st *jobState) {
	var slot *time.Time
	a.mu.Lock()
	if !st.NextSlot.IsZero() {
		s := st.NextSlot
		slot = &s
	}
	delay := st.NextDelayS
	a.mu.Unlock()
	start := a.now()
	if !a.runBackup(ctx, j, "", slot, delay) {
		return // shutdown mid-run: the marker stays and the next start reports "interrupted"
	}
	end := a.now()
	loc, err := j.Schedule.Location(time.Local)
	if err != nil {
		loc = time.Local
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	from := start
	if slot != nil && slot.Before(start) {
		from = *slot
	}
	// Every slot up to now is handled: missed ones were caught up by this run (once), and ones that
	// came due during it are skipped.
	passed := j.Schedule.Slots(from, end, loc)
	for _, s := range passed {
		if s.After(start) {
			s := s
			a.spool(proto.Event{Kind: proto.KindBackup, JobID: j.ID, Status: proto.StatusSkipped, ScheduledFor: &s, Started: s, Finished: s,
				Revision: a.revision(), Error: "the previous run of this job was still going"})
		}
	}
	st.LastSlot = end
	if len(passed) > 0 {
		st.LastSlot = passed[len(passed)-1]
	} else if slot != nil {
		st.LastSlot = *slot
	}
	if st.LastSlot.Before(end) && slot == nil {
		st.LastSlot = end // first run: the schedule starts from now
	}
	st.NextSlot, st.NextDelayS = time.Time{}, 0
	a.saveSchedule()
}

func (a *Agent) revision() int64 {
	if a.hc == nil {
		return 0
	}
	return a.hc.Revision
}

func (a *Agent) runner() restic.Runner {
	env := []string{}
	if b, err := os.ReadFile(filepath.Join(a.cfg.CredentialsDir, "s3.env")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) != "" {
				env = append(env, strings.TrimSpace(line))
			}
		}
	}
	repo := ""
	if a.hc != nil {
		repo = a.hc.Repository
	}
	return restic.Runner{Bin: a.cfg.ResticBin, Repo: repo, PasswordFile: filepath.Join(a.cfg.CredentialsDir, "repo.pass"),
		CacheDir: a.path("cache"), Env: env, RetryLock: a.cfg.RetryLock}
}

func (a *Agent) mark(m runMarker) {
	b, _ := json.Marshal(m)
	if err := writeFileAtomic(a.path("running.json"), b, 0o600); err != nil {
		a.log.Error("writing the run marker", "err", err)
	}
}

// runBackup runs one backup and spools its event. It returns false if the agent is shutting down.
func (a *Agent) runBackup(ctx context.Context, j proto.JobConfig, commandID string, slot *time.Time, delay int) bool {
	started := a.now().UTC()
	a.mu.Lock()
	rev := a.revision()
	r := a.runner()
	st := a.sched[j.ID]
	if st != nil {
		st.LastAttempt = started
	}
	a.running = &proto.Running{Kind: proto.KindBackup, JobID: j.ID, Since: started}
	a.saveSchedule()
	a.mu.Unlock()
	a.mark(runMarker{Kind: proto.KindBackup, JobID: j.ID, CommandID: commandID, Started: started, Revision: rev})

	// The agent's own state (restic cache, spool, token) is never backed up (plan-m1 Q13).
	excludes := append(append([]string(nil), j.Excludes...), a.cfg.StateDir)
	res, err := r.Backup(ctx, restic.BackupOptions{Paths: j.Paths, Excludes: excludes, Host: a.cfg.FQDN, Tags: []string{"job=" + j.ID}})
	if ctx.Err() != nil {
		return false
	}
	ev := proto.Event{Kind: proto.KindBackup, JobID: j.ID, CommandID: commandID, ScheduledFor: slot, SpreadDelayS: delay,
		Started: started, Finished: a.now().UTC(), Revision: rev}
	switch {
	case err != nil:
		ev.Status, ev.Error, ev.ResticExit = proto.StatusFailed, err.Error(), restic.ExitCode(err)
	case res.Incomplete:
		ev.Status, ev.Error, ev.ResticExit = proto.StatusWarning, res.Warnings, restic.ExitIncomplete
	default:
		ev.Status = proto.StatusSuccess
	}
	if err == nil {
		ev.SnapshotID, ev.FilesNew, ev.FilesChanged, ev.BytesAdded = res.SnapshotID, res.FilesNew, res.FilesChanged, res.DataAdded
		ev.TotalFiles, ev.TotalBytes = res.TotalFilesProcessed, res.TotalBytesProcessed
	}
	a.mu.Lock()
	if st != nil {
		st.LastStatus = ev.Status
	}
	a.running = nil
	a.saveSchedule()
	a.mu.Unlock()
	a.spool(ev)
	_ = os.Remove(a.path("running.json"))
	return true
}

// recoverInterrupted turns a leftover run marker (agent killed, host crashed) into an explicit
// "interrupted" event, then removes stale restic locks only.
func (a *Agent) recoverInterrupted(ctx context.Context) {
	b, err := os.ReadFile(a.path("running.json"))
	if err != nil {
		return
	}
	var m runMarker
	if json.Unmarshal(b, &m) == nil {
		a.spool(proto.Event{Kind: m.Kind, JobID: m.JobID, CommandID: m.CommandID, Status: proto.StatusInterrupted, Started: m.Started,
			Finished: a.now().UTC(), Revision: m.Revision, Error: "the agent stopped before the run finished (crash, reboot or kill)"})
		a.mu.Lock()
		if st := a.sched[m.JobID]; st != nil {
			st.LastStatus = proto.StatusInterrupted
			a.saveSchedule()
		}
		a.mu.Unlock()
	}
	_ = os.Remove(a.path("running.json"))
	a.mu.Lock()
	r := a.runner()
	a.mu.Unlock()
	if r.Repo != "" {
		if err := r.Unlock(ctx); err != nil {
			a.log.Warn("removing stale locks failed", "err", err)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
