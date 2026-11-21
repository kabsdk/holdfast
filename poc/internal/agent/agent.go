// Package agent implements holdfast-agent: a self-sufficient executor of its cached
// desired state. The server configures it; it does not need the server to run backups.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"holdfast/internal/proto"
	"holdfast/internal/restic"
)

// Config is the agent's local bootstrap configuration (/etc/holdfast/agent.json).
type Config struct {
	Server      string `json:"server"`       // https://holdfast.example.com
	Host        string `json:"host"`         // stable host identity
	EnrollCode  string `json:"enroll_code"`  // single-use; ignored once a token exists
	StateDir    string `json:"state_dir"`    // /var/lib/holdfast-agent
	ResticBin   string `json:"restic_bin"`   // optional, default "restic" on PATH
	PollTimeout string `json:"poll_timeout"` // default 25s
	TickEvery   string `json:"tick_every"`   // scheduler resolution, default 1s
}

// LoadConfig reads a JSON config file.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Host == "" || c.StateDir == "" || c.Server == "" {
		return c, errors.New("server, host and state_dir are required")
	}
	return c, nil
}

type jobState struct {
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success"`
	LastStatus  string    `json:"last_status"`
}

type runMarker struct {
	Kind      string    `json:"kind"`
	JobID     string    `json:"job_id,omitempty"`
	CommandID string    `json:"command_id,omitempty"`
	Started   time.Time `json:"started"`
}

// Agent is one running agent process.
type Agent struct {
	cfg    Config
	client *http.Client
	logger *log.Logger

	mu       sync.Mutex
	hc       *proto.HostConfig
	sched    map[string]*jobState
	token    string
	commands []proto.Command

	flushNow chan struct{}
	wake     chan struct{}
}

// New prepares an agent; it does not contact the server.
func New(cfg Config, logger *log.Logger) (*Agent, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "holdfast-agent ", log.LstdFlags|log.Lmsgprefix)
	}
	for _, d := range []string{cfg.StateDir, filepath.Join(cfg.StateDir, "spool"), filepath.Join(cfg.StateDir, "cache")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	a := &Agent{
		cfg:      cfg,
		client:   &http.Client{},
		logger:   logger,
		sched:    map[string]*jobState{},
		flushNow: make(chan struct{}, 1),
		wake:     make(chan struct{}, 1),
	}
	a.loadState()
	return a, nil
}

func (a *Agent) path(name string) string { return filepath.Join(a.cfg.StateDir, name) }

func (a *Agent) loadState() {
	if b, err := os.ReadFile(a.path("token")); err == nil {
		a.token = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(a.path("config.json")); err == nil {
		var hc proto.HostConfig
		if json.Unmarshal(b, &hc) == nil {
			a.hc = &hc
		}
	}
	if b, err := os.ReadFile(a.path("schedule.json")); err == nil {
		_ = json.Unmarshal(b, &a.sched)
	}
}

// writeFileAtomic writes via temp file + rename so a crash never leaves half a file.
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
		a.logger.Printf("ERROR saving schedule: %v", err)
	}
}

func (a *Agent) runner() restic.Runner {
	return restic.Runner{
		Bin:          a.cfg.ResticBin,
		Repo:         a.hc.Repository,
		PasswordFile: a.path("password"),
		CacheDir:     a.path("cache"),
	}
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// spool persists an event until the server acknowledges it.
func (a *Agent) spool(ev proto.RunEvent) {
	if ev.ID == "" {
		ev.ID = newID()
	}
	ev.Host = a.cfg.Host
	b, _ := json.Marshal(ev)
	name := fmt.Sprintf("%d-%s.json", time.Now().UnixNano(), ev.ID)
	if err := writeFileAtomic(filepath.Join(a.path("spool"), name), b, 0o600); err != nil {
		a.logger.Printf("ERROR spooling event %s: %v", ev.ID, err)
	}
	a.logger.Printf("run %s kind=%s job=%s status=%s snapshot=%s err=%q", ev.ID, ev.Kind, ev.JobID, ev.Status, ev.SnapshotID, ev.Error)
	select {
	case a.flushNow <- struct{}{}:
	default:
	}
}

// Run starts the agent and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.ensureEnrolled(ctx); err != nil && a.hc == nil {
		// Without a token AND without cached config there is nothing to execute yet.
		return fmt.Errorf("enrollment: %w", err)
	}
	a.recoverInterrupted(ctx)
	// Best effort: pick up config changes made while the agent was down before running
	// anything. If the server is unreachable we proceed with the cached config.
	if a.token != "" {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := a.pollOnce(sctx, 0); err != nil {
			a.logger.Printf("WARN startup sync: %v (using cached config)", err)
		}
		cancel()
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.pollLoop(ctx) }()
	go func() { defer wg.Done(); a.flushLoop(ctx) }()
	go func() { defer wg.Done(); a.workLoop(ctx) }()
	wg.Wait()
	return nil
}

func (a *Agent) ensureEnrolled(ctx context.Context) error {
	if a.token != "" {
		return nil
	}
	if a.cfg.EnrollCode == "" {
		return errors.New("no token and no enroll_code")
	}
	body, _ := json.Marshal(proto.EnrollRequest{Host: a.cfg.Host, Code: a.cfg.EnrollCode})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Server+"/api/v1/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("enroll: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var er proto.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return err
	}
	if err := writeFileAtomic(a.path("token"), []byte(er.Token), 0o600); err != nil {
		return err
	}
	a.token = er.Token
	a.logger.Printf("enrolled as %s", a.cfg.Host)
	return nil
}

// recoverInterrupted turns a leftover run marker (agent killed mid-run, host crashed)
// into an explicit "interrupted" event, then clears stale restic locks.
func (a *Agent) recoverInterrupted(ctx context.Context) {
	b, err := os.ReadFile(a.path("running.json"))
	if err != nil {
		return
	}
	var m runMarker
	if json.Unmarshal(b, &m) == nil {
		a.spool(proto.RunEvent{
			Kind: m.Kind, JobID: m.JobID, CommandID: m.CommandID, Status: proto.StatusInterrupted,
			Started: m.Started, Finished: time.Now().UTC(),
			Error: "agent stopped before the run finished (crash, reboot or kill)",
		})
		if m.JobID != "" {
			a.mu.Lock()
			js := a.jobState(m.JobID)
			js.LastStatus = proto.StatusInterrupted
			a.saveSchedule()
			a.mu.Unlock()
		}
	}
	_ = os.Remove(a.path("running.json"))
	if a.hc != nil {
		if err := a.runner().Unlock(ctx); err != nil {
			a.logger.Printf("WARN stale-lock cleanup: %v", err)
		}
	}
}

func (a *Agent) jobState(id string) *jobState {
	js, ok := a.sched[id]
	if !ok {
		js = &jobState{}
		a.sched[id] = js
	}
	return js
}

func (a *Agent) pollTimeout() time.Duration {
	if d, err := time.ParseDuration(a.cfg.PollTimeout); err == nil && d > 0 {
		return d
	}
	return 25 * time.Second
}

// pollLoop long-polls for config changes and commands. Failures only delay; they never
// stop scheduled work, which runs from the cached config.
func (a *Agent) pollLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if a.token == "" {
			if err := a.ensureEnrolled(ctx); err != nil {
				a.logger.Printf("WARN enroll: %v", err)
				sleep(ctx, backoff)
				backoff = minDur(backoff*2, 30*time.Second)
				continue
			}
		}
		if err := a.pollOnce(ctx, a.pollTimeout()); err != nil {
			if ctx.Err() == nil {
				a.logger.Printf("WARN poll: %v (scheduled backups continue from cached config)", err)
			}
			sleep(ctx, backoff)
			backoff = minDur(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
	}
}

func (a *Agent) pollOnce(ctx context.Context, wait time.Duration) error {
	var version int64
	a.mu.Lock()
	if a.hc != nil {
		version = a.hc.Version
	}
	a.mu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, wait+10*time.Second)
	defer cancel()
	q := url.Values{"version": {fmt.Sprint(version)}, "wait": {wait.String()}}
	req, _ := http.NewRequestWithContext(pctx, http.MethodGet, a.cfg.Server+"/api/v1/agent/poll?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var pr proto.PollResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return err
	}
	if pr.Config != nil {
		if err := a.applyConfig(*pr.Config); err != nil {
			return err
		}
	}
	if len(pr.Commands) > 0 {
		a.mu.Lock()
		a.commands = append(a.commands, pr.Commands...)
		a.mu.Unlock()
		select {
		case a.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (a *Agent) applyConfig(hc proto.HostConfig) error {
	if err := writeFileAtomic(a.path("password"), []byte(hc.Password), 0o600); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(hc, "", "  ")
	if err := writeFileAtomic(a.path("config.json"), b, 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	a.hc = &hc
	a.mu.Unlock()
	a.logger.Printf("applied config version %d (%d jobs)", hc.Version, len(hc.Jobs))
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return nil
}

// flushLoop delivers spooled events; on failure they stay on disk for the next attempt.
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
			a.logger.Printf("WARN report: %v (events kept in spool)", err)
		}
	}
}

func (a *Agent) flush(ctx context.Context) error {
	if a.token == "" {
		return nil
	}
	dir := a.path("spool")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	var events []proto.RunEvent
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var ev proto.RunEvent
		if json.Unmarshal(b, &ev) != nil {
			continue
		}
		names = append(names, e.Name())
		events = append(events, ev)
	}
	if len(events) == 0 {
		return nil
	}
	body, _ := json.Marshal(proto.ReportRequest{Events: events})
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, a.cfg.Server+"/api/v1/agent/report", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("report: %s", resp.Status)
	}
	var rr proto.ReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return err
	}
	acked := map[string]bool{}
	for _, id := range rr.Acked {
		acked[id] = true
	}
	for i, ev := range events {
		if acked[ev.ID] {
			_ = os.Remove(filepath.Join(dir, names[i]))
		}
	}
	return nil
}

func (a *Agent) tick() time.Duration {
	if d, err := time.ParseDuration(a.cfg.TickEvery); err == nil && d > 0 {
		return d
	}
	return time.Second
}

// workLoop runs one thing at a time: queued commands first, then due jobs.
func (a *Agent) workLoop(ctx context.Context) {
	t := time.NewTicker(a.tick())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.wake:
		}
		for ctx.Err() == nil {
			if cmd, ok := a.nextCommand(); ok {
				a.execCommand(ctx, cmd)
				continue
			}
			job, ok := a.nextDueJob(time.Now())
			if !ok {
				break
			}
			a.runBackup(ctx, job, "")
		}
	}
}

func (a *Agent) nextCommand() (proto.Command, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.commands) == 0 || a.hc == nil {
		return proto.Command{}, false
	}
	c := a.commands[0]
	a.commands = a.commands[1:]
	return c, true
}

// nextDueJob picks the most overdue job. A job that never ran is due immediately,
// so a fresh install or a reboot after a long outage catches up at once.
func (a *Agent) nextDueJob(now time.Time) (proto.Job, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hc == nil {
		return proto.Job{}, false
	}
	type cand struct {
		job proto.Job
		due time.Time
	}
	var cands []cand
	for _, j := range a.hc.Jobs {
		js := a.jobState(j.ID)
		due := js.LastAttempt.Add(j.Interval.Duration)
		failed := js.LastStatus == proto.StatusFailed || js.LastStatus == proto.StatusInterrupted
		if failed && j.Retry.Duration > 0 && j.Retry.Duration < j.Interval.Duration {
			due = js.LastAttempt.Add(j.Retry.Duration)
		}
		if js.LastAttempt.IsZero() || !now.Before(due) {
			cands = append(cands, cand{j, due})
		}
	}
	if len(cands) == 0 {
		return proto.Job{}, false
	}
	sort.Slice(cands, func(i, k int) bool { return cands[i].due.Before(cands[k].due) })
	return cands[0].job, true
}

func (a *Agent) mark(m runMarker) {
	b, _ := json.Marshal(m)
	if err := writeFileAtomic(a.path("running.json"), b, 0o600); err != nil {
		a.logger.Printf("ERROR writing run marker: %v", err)
	}
}

func (a *Agent) runBackup(ctx context.Context, job proto.Job, commandID string) {
	started := time.Now().UTC()
	a.mark(runMarker{Kind: "backup", JobID: job.ID, CommandID: commandID, Started: started})
	a.mu.Lock()
	js := a.jobState(job.ID)
	js.LastAttempt = started
	a.saveSchedule()
	r := a.runner()
	host := a.cfg.Host
	a.mu.Unlock()

	res, err := r.Backup(ctx, restic.BackupOptions{
		Paths: job.Paths, Excludes: job.Excludes, Host: host,
		Tags: []string{"holdfast", "job:" + job.ID}, ExtraArgs: job.ExtraArgs,
	})
	if ctx.Err() != nil {
		// Shutdown mid-run: leave the marker so the next start reports "interrupted".
		return
	}
	ev := proto.RunEvent{Kind: "backup", JobID: job.ID, CommandID: commandID, Started: started, Finished: time.Now().UTC()}
	switch {
	case err != nil:
		ev.Status, ev.Error = proto.StatusFailed, err.Error()
	case res.Incomplete:
		ev.Status, ev.Error = proto.StatusWarning, res.Warnings
	default:
		ev.Status = proto.StatusSuccess
	}
	if err == nil {
		ev.SnapshotID, ev.FilesNew, ev.BytesAdded = res.SnapshotID, res.FilesNew, res.DataAdded
	}
	a.mu.Lock()
	js.LastStatus = ev.Status
	if ev.Status == proto.StatusSuccess || ev.Status == proto.StatusWarning {
		js.LastSuccess = ev.Finished
	}
	a.saveSchedule()
	a.mu.Unlock()
	a.spool(ev)
	_ = os.Remove(a.path("running.json"))
}

func (a *Agent) execCommand(ctx context.Context, c proto.Command) {
	switch c.Kind {
	case "backup_now":
		a.mu.Lock()
		var job *proto.Job
		for i := range a.hc.Jobs {
			if a.hc.Jobs[i].ID == c.JobID {
				job = &a.hc.Jobs[i]
			}
		}
		a.mu.Unlock()
		if job == nil {
			a.spool(proto.RunEvent{Kind: "backup", JobID: c.JobID, CommandID: c.ID, Status: proto.StatusFailed,
				Started: time.Now().UTC(), Finished: time.Now().UTC(), Error: "unknown job"})
			return
		}
		a.runBackup(ctx, *job, c.ID)
	case "restore":
		started := time.Now().UTC()
		a.mark(runMarker{Kind: "restore", CommandID: c.ID, Started: started})
		a.mu.Lock()
		r := a.runner()
		a.mu.Unlock()
		ev := proto.RunEvent{Kind: "restore", CommandID: c.ID, SnapshotID: c.Snapshot, Started: started}
		var err error
		if c.Target == "" || !filepath.IsAbs(c.Target) {
			err = errors.New("restore target must be an absolute path")
		} else {
			err = r.Restore(ctx, c.Snapshot, c.Target, c.Include)
		}
		if ctx.Err() != nil {
			return
		}
		ev.Finished = time.Now().UTC()
		if err != nil {
			ev.Status, ev.Error = proto.StatusFailed, err.Error()
		} else {
			ev.Status = proto.StatusSuccess
		}
		a.spool(ev)
		_ = os.Remove(a.path("running.json"))
	default:
		a.spool(proto.RunEvent{Kind: c.Kind, CommandID: c.ID, Status: proto.StatusFailed,
			Started: time.Now().UTC(), Finished: time.Now().UTC(), Error: "unknown command kind"})
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

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
