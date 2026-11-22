// Package agent is holdfast-agent: it runs its cached config on its own, whether the server is
// reachable or not (decision 006). The server configures it; it never needs the server to back up.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
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
	// A failed scheduled backup is retried before the job's next slot (plan-m5 Q4): Retries made so far,
	// and when the next one is due; zero: none is.
	Retries   int       `json:"retries,omitempty"`
	RetryAt   time.Time `json:"retry_at,omitzero"`
	retryable bool      // the last run failed in a way a retry can fix: not stopped by its timeout
}

// RetryAfter are the waits before each retry of a failed scheduled backup (plan-m5 Q4).
var RetryAfter = []time.Duration{5 * time.Minute, 15 * time.Minute, 45 * time.Minute}

type runMarker struct {
	Kind      string    `json:"kind"`
	JobID     string    `json:"job_id,omitempty"`
	CommandID string    `json:"command_id,omitempty"`
	Started   time.Time `json:"started"`
	Revision  int64     `json:"revision"`
}

// Agent is one running agent.
type Agent struct {
	cfg        Config
	client     *http.Client
	log        *slog.Logger
	now        func() time.Time
	runTimeout func(schedule.Schedule) time.Duration // RunTimeout; tests shorten it

	mu        sync.Mutex
	token     string
	hc        *proto.HostConfig
	credVer   int
	sched     map[string]*jobState
	toAck     []string
	done      map[string]time.Time // command IDs already run, so a re-sent command runs once
	running   *proto.Running
	lastCheck time.Time // when the last repository check started
	factsOnce sync.Once
	base      proto.Facts // facts that do not change while the agent runs

	warnedNewer bool // logged once: the server is older than this agent (only the poll loop sets it)

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
	client, err := httpClient(cfg.Server, cfg.CASHA256)
	if err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, client: client, log: log, now: time.Now, runTimeout: RunTimeout, sched: map[string]*jobState{}, done: map[string]time.Time{},
		wake: make(chan struct{}, 1), flushNow: make(chan struct{}, 1)}
	a.load()
	return a, nil
}

// httpClient pins the server when a fingerprint is given. Without one, the system's roots apply.
func httpClient(server, pin string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if pin != "" {
		u, err := url.Parse(server)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("server %q is not a URL", server)
		}
		host := u.Hostname()
		pin = strings.ToLower(strings.ReplaceAll(pin, ":", ""))
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // the standard verification is replaced by verifyPinned
			VerifyConnection: func(cs tls.ConnectionState) error {
				return verifyPinned(cs.PeerCertificates, pin, host)
			},
		}
	}
	return &http.Client{Transport: tr}, nil
}

// verifyPinned accepts a server whose own certificate chains to the pinned certificate and names host.
//
// The pin is the sha256 of one certificate in the chain the server sends: normally its CA's, or its own
// if it is self-signed. That certificate becomes the only trusted root. Finding the pin somewhere in the
// chain is not enough on its own. A CA's certificate is public, so a man in the middle could send it
// along with a certificate of his own (decision 030).
func verifyPinned(chain []*x509.Certificate, pin, host string) error {
	if len(chain) == 0 {
		return errors.New("the server sent no certificate")
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	found := false
	for _, c := range chain {
		sum := sha256.Sum256(c.Raw)
		if hex.EncodeToString(sum[:]) == pin {
			roots.AddCert(c)
			found = true
		} else {
			intermediates.AddCert(c)
		}
	}
	if !found {
		return errors.New("the server's certificate chain does not match the pinned fingerprint")
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: host}); err != nil {
		return fmt.Errorf("the server's certificate does not chain to the pinned certificate for %s: %w", host, err)
	}
	return nil
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
	if b, err := os.ReadFile(a.path("check.json")); err == nil {
		var c struct {
			Last time.Time `json:"last"`
		}
		if json.Unmarshal(b, &c) == nil {
			a.lastCheck = c.Last
		}
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
			err = fmt.Errorf("%s: %s", resp.Status, e.Error)
		} else {
			err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		if resp.StatusCode == http.StatusUpgradeRequired {
			return &tooOldError{err}
		}
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// tooOldError is the server's answer to an agent older than it accepts (plan-m5 item 1). The agent goes on
// backing up from its cached config, and asks again now and then.
type tooOldError struct{ error }

// tooOldWait is how long the agent waits before asking a server that found it too old again.
var tooOldWait = 5 * time.Minute

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
	bounds := a.cfg.Restore
	f.RestoreBounds = &bounds
	return f
}

func (a *Agent) readFacts(ctx context.Context) proto.Facts {
	f := proto.Facts{Protocol: proto.Protocol, AgentVersion: Version}
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
			var old *tooOldError
			switch {
			case ctx.Err() != nil:
			case errors.As(err, &old):
				a.log.Error("the server needs a newer agent: upgrade holdfast-agent. Scheduled backups continue from the cached config", "err", err)
				sleep(ctx, tooOldWait)
				continue
			default:
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
	if p := resp.Protocol; p != nil && p.Max < proto.Protocol && !a.warnedNewer {
		a.warnedNewer = true
		a.log.Warn("this agent is newer than the server: upgrade the server first", "agent_protocol", proto.Protocol, "server_max", p.Max)
	}
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
	if err := writeFileAtomic(a.path("credentials.sha256"), []byte(credentialsDigest([]byte(c.RepoPassword), []byte(env))), 0o600); err != nil {
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

func credentialsDigest(pass, env []byte) string {
	sum := sha256.Sum256(append(append(append([]byte{}, pass...), 0), env...))
	return hex.EncodeToString(sum[:])
}

// verifyCredentials checks, before each run, that the credential files still hold what the server sent
// (decision 032). They are Holdfast's own files. If someone changed them, the agent records it in the
// host's history and asks the server for them again: its next poll reports version 0, and the server
// sends the current ones. The run goes ahead, and its result shows whether the changed files worked.
func (a *Agent) verifyCredentials() {
	want, err := os.ReadFile(a.path("credentials.sha256"))
	if err != nil {
		return // credentials not received yet, or written by an agent from before this check
	}
	pass, err1 := os.ReadFile(filepath.Join(a.cfg.CredentialsDir, "repo.pass"))
	env, err2 := os.ReadFile(filepath.Join(a.cfg.CredentialsDir, "s3.env"))
	if errors.Join(err1, err2) == nil && credentialsDigest(pass, env) == strings.TrimSpace(string(want)) {
		return
	}
	a.mu.Lock()
	already := a.credVer == 0
	a.credVer = 0
	a.mu.Unlock()
	if already {
		return // reported once; the server's answer will repair them
	}
	_ = writeFileAtomic(a.path("credentials.version"), []byte("0"), 0o600)
	now := a.now().UTC()
	a.spool(proto.Event{Kind: proto.KindNotice, Status: proto.StatusWarning, Started: now, Finished: now, Revision: a.revision(),
		Error: "the credential files in " + a.cfg.CredentialsDir + " were changed or removed on the host; the agent is fetching them again from the server"})
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

// due returns the most overdue job, if any. A slot missed while the agent was down is due at once and runs
// once, however many slots were missed (plan-m1 Q7).
func (a *Agent) due(now time.Time) (proto.JobConfig, *jobState, bool) {
	j, st, _, ok := a.dueRun(now)
	return j, st, ok
}

// dueRun is due, and says whether the run is a retry of a failed slot (plan-m5 Q4).
func (a *Agent) dueRun(now time.Time) (proto.JobConfig, *jobState, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hc == nil {
		return proto.JobConfig{}, nil, false, false
	}
	type cand struct {
		j     proto.JobConfig
		st    *jobState
		at    time.Time
		retry bool
	}
	var cands []cand
	for _, j := range a.hc.Jobs {
		st := a.plan(j, now)
		if st.LastSlot.IsZero() {
			// Never completed a slot: the first backup, or a first backup that was interrupted.
			cands = append(cands, cand{j, st, time.Time{}, false})
			continue
		}
		if !st.NextSlot.IsZero() {
			if at := st.NextSlot.Add(time.Duration(st.NextDelayS) * time.Second); !now.Before(at) {
				cands = append(cands, cand{j, st, at, false})
				continue
			}
		}
		if !st.RetryAt.IsZero() && !now.Before(st.RetryAt) {
			cands = append(cands, cand{j, st, st.RetryAt, true})
		}
	}
	a.saveSchedule()
	if len(cands) == 0 {
		return proto.JobConfig{}, nil, false, false
	}
	sort.Slice(cands, func(i, k int) bool { return cands[i].at.Before(cands[k].at) })
	return cands[0].j, cands[0].st, cands[0].retry, true
}

// planRetry schedules the next retry of a job whose run just failed, if a retry can help and still starts
// before the job's next slot; otherwise it clears the retries. Caller holds a.mu.
func (a *Agent) planRetry(j proto.JobConfig, st *jobState, end time.Time) {
	if st.LastStatus != proto.StatusFailed || !st.retryable || st.Retries >= len(RetryAfter) {
		st.Retries, st.RetryAt = 0, time.Time{}
		return
	}
	at := end.Add(RetryAfter[st.Retries])
	loc, err := j.Schedule.Location(time.Local)
	if err != nil {
		loc = time.Local
	}
	if next, ok := j.Schedule.Next(st.LastSlot, loc); ok && !at.Before(next) {
		st.Retries, st.RetryAt = 0, time.Time{} // the next slot comes first, and runs the job anyway
		return
	}
	st.RetryAt = at
}

// runRetry retries a failed scheduled backup. The schedule is not advanced: the retry belongs to the slot
// that failed.
func (a *Agent) runRetry(ctx context.Context, j proto.JobConfig, st *jobState) {
	a.mu.Lock()
	st.Retries++
	n := st.Retries
	st.RetryAt = time.Time{}
	a.saveSchedule()
	a.mu.Unlock()
	if !a.runBackup(ctx, j, "", nil, 0, n) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planRetry(j, st, a.now())
	a.saveSchedule()
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
			j, st, retry, ok := a.dueRun(a.now())
			if !ok {
				break
			}
			if retry {
				a.runRetry(ctx, j, st)
			} else {
				a.runScheduled(ctx, j, st)
			}
			a.checkIfDue(ctx, st)
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
		a.runBackup(ctx, *job, c.ID, nil, 0, 0)
	case proto.CommandCheckNow:
		a.runCheck(ctx, c.ID)
	case proto.CommandRestore:
		a.runRestore(ctx, c)
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
	if !a.runBackup(ctx, j, "", slot, delay, 0) {
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
	st.Retries = 0 // a new slot starts its own retries
	a.planRetry(j, st, end)
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

// runBackup runs one backup and spools its event. retry is which retry this is, 0 for none. It returns false
// if the agent is shutting down.
func (a *Agent) runBackup(ctx context.Context, j proto.JobConfig, commandID string, slot *time.Time, delay, retry int) bool {
	a.verifyCredentials()
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
	timeout := a.runTimeout(j.Schedule)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	res, err := r.Backup(rctx, restic.BackupOptions{Paths: j.Paths, Excludes: excludes, Host: a.cfg.FQDN, Tags: []string{"job=" + j.ID},
		LimitUploadKiB: j.LimitUploadKiB})
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	ev := proto.Event{Kind: proto.KindBackup, JobID: j.ID, CommandID: commandID, ScheduledFor: slot, SpreadDelayS: delay,
		Started: started, Finished: a.now().UTC(), Revision: rev}
	if retry > 0 {
		ev.Detail = map[string]string{"retry": fmt.Sprintf("%d of %d", retry, len(RetryAfter))}
	}
	switch {
	case timedOut:
		ev.Status, ev.ResticExit = proto.StatusFailed, restic.ExitCode(err)
		ev.Error = fmt.Sprintf("the backup ran for %s, its schedule's longest gap, and was stopped so the host's other jobs can run", timeout)
		a.unlockStale(ctx)
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
		st.retryable = !timedOut // a run stopped at its timeout already took the whole gap
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
	a.unlockStale(ctx)
}

/* ---------------- the repository check (decision 030) ---------------- */

// checkIfDue runs the daily repository check after a backup that succeeded, at most once in 23 hours.
// It runs in the agent's one-restic-at-a-time loop. restic check takes an exclusive lock, but here it can
// never meet this host's own backups.
func (a *Agent) checkIfDue(ctx context.Context, st *jobState) {
	a.mu.Lock()
	on := a.hc != nil && a.hc.Check != nil && a.hc.Check.CycleDays > 0
	ok := st != nil && (st.LastStatus == proto.StatusSuccess || st.LastStatus == proto.StatusWarning)
	last := a.lastCheck
	a.mu.Unlock()
	if on && ok && a.now().Sub(last) >= 23*time.Hour {
		a.runCheck(ctx, "")
	}
}

// checkTimeout bounds a check: the shortest run timeout of the host's jobs, so a slow check delays the
// most frequent job by at most one of its gaps.
func (a *Agent) checkTimeout() time.Duration {
	t := 24 * time.Hour
	if a.hc != nil {
		for _, j := range a.hc.Jobs {
			t = min(t, a.runTimeout(j.Schedule))
		}
	}
	return t
}

// runCheck runs restic check --read-data-subset n/cycle, with n turning daily, and reports it. A check
// that fails is reported as failed: the repository may be damaged. It returns false on shutdown.
func (a *Agent) runCheck(ctx context.Context, commandID string) bool {
	a.verifyCredentials()
	started := a.now().UTC()
	a.mu.Lock()
	cycle := 30 // a check on request reads a subset too, even with daily checks off
	if a.hc != nil && a.hc.Check != nil && a.hc.Check.CycleDays > 0 {
		cycle = a.hc.Check.CycleDays
	}
	r, rev, timeout := a.runner(), a.revision(), a.checkTimeout()
	a.lastCheck = started
	a.running = &proto.Running{Kind: proto.KindCheck, Since: started}
	b, _ := json.Marshal(map[string]time.Time{"last": started})
	a.mu.Unlock()
	_ = writeFileAtomic(a.path("check.json"), b, 0o600)
	a.mark(runMarker{Kind: proto.KindCheck, CommandID: commandID, Started: started, Revision: rev})

	subset := fmt.Sprintf("%d/%d", started.YearDay()%cycle+1, cycle)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	err := r.Check(rctx, subset)
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	ev := proto.Event{Kind: proto.KindCheck, CommandID: commandID, Status: proto.StatusSuccess, Started: started, Finished: a.now().UTC(), Revision: rev}
	switch {
	case timedOut:
		ev.Status, ev.ResticExit = proto.StatusFailed, restic.ExitCode(err)
		ev.Error = fmt.Sprintf("the check of subset %s ran for %s and was stopped", subset, timeout)
		a.unlockStale(ctx)
	case err != nil:
		ev.Status, ev.ResticExit, ev.Error = proto.StatusFailed, restic.ExitCode(err), "check of subset "+subset+": "+err.Error()
	}
	a.mu.Lock()
	a.running = nil
	a.mu.Unlock()
	a.spool(ev)
	_ = os.Remove(a.path("running.json"))
	return true
}

// unlockStale removes stale restic locks only (never --remove-all), after a run that did not end
// normally.
func (a *Agent) unlockStale(ctx context.Context) {
	a.mu.Lock()
	r := a.runner()
	a.mu.Unlock()
	if r.Repo == "" {
		return
	}
	if out, err := r.Unlock(ctx); err != nil {
		a.log.Warn("removing stale locks failed", "err", err)
	} else {
		a.log.Info("removed stale locks after a run that did not finish", "restic", out)
	}
}

// RunTimeout is how long one backup may run: its schedule's longest gap, and at least an hour. A
// longer run would only skip its own next slots, and it would hold up the host's other jobs, since the
// agent runs one restic at a time (decision 030).
func RunTimeout(s schedule.Schedule) time.Duration {
	return max(s.LongestGap(), time.Hour)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
