// Package server implements holdfast-server: the configurator and coordinator.
//
// It owns desired state, per-host credentials, restore orchestration and repository
// maintenance (forget/prune/check). It does not schedule file backups: agents do that
// from their cached config, so a server outage never stops backups.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"holdfast/internal/proto"
	"holdfast/internal/restic"
)

// HostSpec is one host in the desired-state file.
type HostSpec struct {
	Name       string      `json:"name"`
	EnrollCode string      `json:"enroll_code"`
	Jobs       []proto.Job `json:"jobs"`
}

// Config is the server's desired-state file. In production this lives in Git and the
// UI edits it through the API; the file stays the source of truth.
type Config struct {
	Listen           string         `json:"listen"`
	StateDir         string         `json:"state_dir"`
	RepoBase         string         `json:"repo_base"` // PoC: local path. Production: s3:https://rgw/bucket/repos
	ResticBin        string         `json:"restic_bin"`
	AdminToken       string         `json:"admin_token"`
	MaintenanceEvery proto.Duration `json:"maintenance_every"`
	CheckCycleDays   int            `json:"check_cycle_days"` // read-data-subset cycle, default 30
	Hosts            []HostSpec     `json:"hosts"`
}

// LoadConfig reads and validates the desired-state file.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.StateDir == "" || c.RepoBase == "" {
		return c, errors.New("state_dir and repo_base are required")
	}
	if c.CheckCycleDays <= 0 {
		c.CheckCycleDays = 30
	}
	seen := map[string]bool{}
	for _, h := range c.Hosts {
		if h.Name == "" || strings.ContainsAny(h.Name, "/\\ ") || seen[h.Name] {
			return c, fmt.Errorf("invalid or duplicate host name %q", h.Name)
		}
		seen[h.Name] = true
		for _, j := range h.Jobs {
			if j.ID == "" || j.Interval.Duration <= 0 || len(j.Paths) == 0 {
				return c, fmt.Errorf("host %s: job %q needs id, paths and interval", h.Name, j.ID)
			}
			if err := validateExtraArgs(j.ExtraArgs); err != nil {
				return c, fmt.Errorf("host %s job %s: %w", h.Name, j.ID, err)
			}
		}
	}
	return c, nil
}

// Only flags that cannot redirect the repository or leak credentials are allowed.
var allowedExtra = map[string]bool{
	"--one-file-system": true, "--exclude-caches": true, "--limit-upload": true,
	"--read-concurrency": true, "--exclude-larger-than": true, "--no-scan": true,
}

func validateExtraArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue // value of previous flag
		}
		if !allowedExtra[strings.SplitN(a, "=", 2)[0]] {
			return fmt.Errorf("extra arg %s is not allowed", a)
		}
	}
	return nil
}

// JobStatus is the server's view of one job, built only from agent reports.
type JobStatus struct {
	LastRun      time.Time `json:"last_run"`
	LastSuccess  time.Time `json:"last_success"`
	LastStatus   string    `json:"last_status"`
	LastError    string    `json:"last_error,omitempty"`
	LastSnapshot string    `json:"last_snapshot,omitempty"`
}

// MaintStatus records server-side maintenance per host.
type MaintStatus struct {
	LastForget   time.Time `json:"last_forget"`
	LastCheck    time.Time `json:"last_check"`
	LastSubset   string    `json:"last_subset"`
	LastStatus   string    `json:"last_status"`
	LastError    string    `json:"last_error,omitempty"`
	LastSuccess  time.Time `json:"last_success"`
}

// HostState is persisted per host.
type HostState struct {
	TokenHash   string                `json:"token_hash,omitempty"`
	Password    string                `json:"password"` // PoC: plaintext in a 0600 file. Production: envelope-encrypted + escrow.
	Enrolled    time.Time             `json:"enrolled"`
	LastSeen    time.Time             `json:"last_seen"`
	Jobs        map[string]*JobStatus `json:"jobs"`
	Runs        map[string]int64      `json:"runs"` // "kind|job|status" -> count
	Pending     []proto.Command       `json:"pending"`
	Maint       MaintStatus           `json:"maintenance"`
	EnrollUsed  bool                  `json:"enroll_used"`
	RecentRuns  []proto.RunEvent      `json:"recent_runs"`
}

type state struct {
	Hosts      map[string]*HostState `json:"hosts"`
	SeenEvents map[string]time.Time  `json:"seen_events"`
}

// Server is the running control plane.
type Server struct {
	cfg    Config
	logger *log.Logger
	mu     sync.Mutex
	st     state
	specs  map[string]HostSpec
}

// New loads persisted state and reconciles it with the desired-state config.
func New(cfg Config, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "holdfast-server ", log.LstdFlags|log.Lmsgprefix)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, logger: logger, specs: map[string]HostSpec{}}
	s.st = state{Hosts: map[string]*HostState{}, SeenEvents: map[string]time.Time{}}
	if b, err := os.ReadFile(s.statePath()); err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("state file corrupt: %w", err)
		}
	}
	for _, h := range cfg.Hosts {
		s.specs[h.Name] = h
		hs := s.host(h.Name)
		for _, j := range h.Jobs {
			if hs.Jobs[j.ID] == nil {
				hs.Jobs[j.ID] = &JobStatus{}
			}
		}
	}
	return s, s.save()
}

func (s *Server) statePath() string { return filepath.Join(s.cfg.StateDir, "state.json") }

func (s *Server) host(name string) *HostState {
	hs := s.st.Hosts[name]
	if hs == nil {
		hs = &HostState{}
		s.st.Hosts[name] = hs
	}
	if hs.Jobs == nil {
		hs.Jobs = map[string]*JobStatus{}
	}
	if hs.Runs == nil {
		hs.Runs = map[string]int64{}
	}
	return hs
}

// save persists state atomically. Caller may or may not hold mu; save takes a snapshot.
func (s *Server) save() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

// RepoFor returns the per-host repository location.
func (s *Server) RepoFor(host string) string { return filepath.Join(s.cfg.RepoBase, host) }

func (s *Server) passwordFile(host, password string) (string, error) {
	dir := filepath.Join(s.cfg.StateDir, "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, host+".pass")
	return p, os.WriteFile(p, []byte(password), 0o600)
}

// Runner returns the maintenance-identity restic runner for one host's repository.
func (s *Server) Runner(host string) (restic.Runner, error) {
	s.mu.Lock()
	hs := s.st.Hosts[host]
	s.mu.Unlock()
	if hs == nil || hs.Password == "" {
		return restic.Runner{}, fmt.Errorf("host %s has no repository yet", host)
	}
	pf, err := s.passwordFile(host, hs.Password)
	if err != nil {
		return restic.Runner{}, err
	}
	return restic.Runner{Bin: s.cfg.ResticBin, Repo: s.RepoFor(host), PasswordFile: pf,
		CacheDir: filepath.Join(s.cfg.StateDir, "cache")}, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// hostConfig renders the desired state for one host. Version is a content hash, so any
// change in jobs, repository or password is pushed to the agent on its next poll.
func (s *Server) hostConfig(name string, hs *HostState) proto.HostConfig {
	spec := s.specs[name]
	hc := proto.HostConfig{Host: name, Repository: s.RepoFor(name), Password: hs.Password, Jobs: spec.Jobs}
	b, _ := json.Marshal(hc)
	h := fnv.New64a()
	_, _ = h.Write(b)
	hc.Version = int64(h.Sum64() & 0x7fffffffffffffff)
	return hc
}

// Handler returns the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/enroll", s.handleEnroll)
	mux.HandleFunc("GET /api/v1/agent/poll", s.handlePoll)
	mux.HandleFunc("POST /api/v1/agent/report", s.handleReport)
	mux.HandleFunc("GET /api/v1/hosts", s.admin(s.handleHosts))
	mux.HandleFunc("GET /api/v1/hosts/{host}/snapshots", s.admin(s.handleSnapshots))
	mux.HandleFunc("POST /api/v1/hosts/{host}/restore", s.admin(s.handleRestore))
	mux.HandleFunc("POST /api/v1/hosts/{host}/jobs/{job}/run", s.admin(s.handleRunNow))
	mux.HandleFunc("POST /api/v1/maintenance/run", s.admin(s.handleMaintenance))
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func bearer(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" || subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.cfg.AdminToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// agentHost authenticates an agent token and returns its host name.
func (s *Server) agentHost(r *http.Request) (string, bool) {
	th := hashToken(bearer(r))
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, hs := range s.st.Hosts {
		if hs.TokenHash != "" && subtle.ConstantTimeCompare([]byte(hs.TokenHash), []byte(th)) == 1 {
			if _, ok := s.specs[name]; ok {
				return name, true
			}
		}
	}
	return "", false
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req proto.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	spec, ok := s.specs[req.Host]
	hs := s.host(req.Host)
	if !ok || spec.EnrollCode == "" || hs.EnrollUsed ||
		subtle.ConstantTimeCompare([]byte(spec.EnrollCode), []byte(req.Code)) != 1 {
		s.mu.Unlock()
		http.Error(w, "invalid or used enrollment code", http.StatusForbidden)
		return
	}
	if hs.Password == "" {
		hs.Password = randHex(32)
	}
	token := randHex(32)
	hs.TokenHash, hs.EnrollUsed, hs.Enrolled = hashToken(token), true, time.Now().UTC()
	s.mu.Unlock()

	// The server (maintenance identity) creates the repository; agents never need
	// permission to create buckets or repositories.
	rn, err := s.Runner(req.Host)
	if err == nil {
		err = rn.Init(r.Context())
	}
	if err != nil {
		http.Error(w, "repository init failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	_ = s.save()
	s.mu.Unlock()
	s.logger.Printf("enrolled host %s, repository %s", req.Host, s.RepoFor(req.Host))
	writeJSON(w, http.StatusOK, proto.EnrollResponse{Token: token})
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentHost(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var have int64
	fmt.Sscan(r.URL.Query().Get("version"), &have)
	wait, err := time.ParseDuration(r.URL.Query().Get("wait"))
	if err != nil || wait < 0 || wait > 60*time.Second {
		wait = 25 * time.Second // wait=0s means "answer immediately" (agent startup sync)
	}
	deadline := time.Now().Add(wait)
	for {
		s.mu.Lock()
		hs := s.host(name)
		hs.LastSeen = time.Now().UTC()
		hc := s.hostConfig(name, hs)
		var resp proto.PollResponse
		if hc.Version != have {
			resp.Config = &hc
		}
		if len(hs.Pending) > 0 {
			resp.Commands, hs.Pending = hs.Pending, nil
		}
		if resp.Config != nil || len(resp.Commands) > 0 || time.Now().After(deadline) {
			_ = s.save()
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, resp)
			return
		}
		s.mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	name, ok := s.agentHost(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req proto.ReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var acked []string
	s.mu.Lock()
	hs := s.host(name)
	hs.LastSeen = time.Now().UTC()
	for _, ev := range req.Events {
		if ev.ID == "" {
			continue
		}
		acked = append(acked, ev.ID)
		if _, dup := s.st.SeenEvents[ev.ID]; dup {
			continue // idempotent re-delivery
		}
		s.st.SeenEvents[ev.ID] = time.Now().UTC()
		ev.Host = name // never trust the host field from the body
		hs.Runs[ev.Kind+"|"+ev.JobID+"|"+ev.Status]++
		hs.RecentRuns = append([]proto.RunEvent{ev}, hs.RecentRuns...)
		if len(hs.RecentRuns) > 50 {
			hs.RecentRuns = hs.RecentRuns[:50]
		}
		if ev.Kind == "backup" && ev.JobID != "" {
			js := hs.Jobs[ev.JobID]
			if js == nil {
				js = &JobStatus{}
				hs.Jobs[ev.JobID] = js
			}
			if ev.Finished.After(js.LastRun) || js.LastRun.IsZero() {
				js.LastRun, js.LastStatus, js.LastError = ev.Finished, ev.Status, ev.Error
			}
			if (ev.Status == proto.StatusSuccess || ev.Status == proto.StatusWarning) && ev.Finished.After(js.LastSuccess) {
				js.LastSuccess, js.LastSnapshot = ev.Finished, ev.SnapshotID
			}
		}
		s.logger.Printf("report host=%s kind=%s job=%s status=%s snapshot=%s", name, ev.Kind, ev.JobID, ev.Status, ev.SnapshotID)
	}
	// Bound the dedup map.
	if len(s.st.SeenEvents) > 10000 {
		cut := time.Now().Add(-30 * 24 * time.Hour)
		for id, t := range s.st.SeenEvents {
			if t.Before(cut) {
				delete(s.st.SeenEvents, id)
			}
		}
	}
	_ = s.save()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, proto.ReportResponse{Acked: acked})
}

// HostView is the admin API's per-host summary.
type HostView struct {
	Name     string                `json:"name"`
	Enrolled bool                  `json:"enrolled"`
	LastSeen time.Time             `json:"last_seen"`
	Jobs     map[string]*JobStatus `json:"jobs"`
	Maint    MaintStatus           `json:"maintenance"`
	Recent   []proto.RunEvent      `json:"recent_runs"`
}

func (s *Server) handleHosts(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	var out []HostView
	for name := range s.specs {
		hs := s.host(name)
		out = append(out, HostView{Name: name, Enrolled: hs.TokenHash != "", LastSeen: hs.LastSeen, Jobs: hs.Jobs, Maint: hs.Maint, Recent: hs.RecentRuns})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	rn, err := s.Runner(r.PathValue("host"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	snaps, err := rn.Snapshots(r.Context(), "holdfast")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) enqueue(host string, c proto.Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.specs[host]; !ok {
		return errors.New("unknown host")
	}
	c.ID = randHex(8)
	hs := s.host(host)
	hs.Pending = append(hs.Pending, c)
	return s.save()
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req proto.RestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Snapshot == "" || !filepath.IsAbs(req.Target) {
		http.Error(w, "snapshot and absolute target required", http.StatusBadRequest)
		return
	}
	c := proto.Command{Kind: "restore", Snapshot: req.Snapshot, Target: req.Target, Include: req.Include}
	if err := s.enqueue(r.PathValue("host"), c); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) handleRunNow(w http.ResponseWriter, r *http.Request) {
	c := proto.Command{Kind: "backup_now", JobID: r.PathValue("job")}
	if err := s.enqueue(r.PathValue("host"), c); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	res := s.RunMaintenance(r.Context(), time.Now())
	writeJSON(w, http.StatusOK, res)
}

// RunMaintenance applies retention (forget --prune) per job and a rotating
// read-data-subset check per host. Only the leader runs this.
func (s *Server) RunMaintenance(ctx context.Context, now time.Time) map[string]string {
	out := map[string]string{}
	for name, spec := range s.specs {
		rn, err := s.Runner(name)
		if err != nil {
			continue // not enrolled yet
		}
		var errs []string
		for _, j := range spec.Jobs {
			keep := map[string]int{"last": j.Retention.KeepLast, "daily": j.Retention.KeepDaily,
				"weekly": j.Retention.KeepWeekly, "monthly": j.Retention.KeepMonthly, "yearly": j.Retention.KeepYearly}
			if err := rn.Forget(ctx, "job:"+j.ID, keep, true); err != nil {
				errs = append(errs, fmt.Sprintf("forget %s: %v", j.ID, err))
			}
		}
		subset := fmt.Sprintf("%d/%d", now.YearDay()%s.cfg.CheckCycleDays+1, s.cfg.CheckCycleDays)
		if err := rn.Check(ctx, subset); err != nil {
			errs = append(errs, fmt.Sprintf("check %s: %v", subset, err))
		}
		s.mu.Lock()
		hs := s.host(name)
		hs.Maint.LastForget, hs.Maint.LastCheck, hs.Maint.LastSubset = now.UTC(), now.UTC(), subset
		if len(errs) == 0 {
			hs.Maint.LastStatus, hs.Maint.LastError, hs.Maint.LastSuccess = proto.StatusSuccess, "", now.UTC()
		} else {
			hs.Maint.LastStatus, hs.Maint.LastError = proto.StatusFailed, strings.Join(errs, "; ")
		}
		out[name] = hs.Maint.LastStatus
		_ = s.save()
		s.mu.Unlock()
	}
	return out
}

// MaintenanceLoop runs RunMaintenance on the configured interval.
func (s *Server) MaintenanceLoop(ctx context.Context) {
	every := s.cfg.MaintenanceEvery.Duration
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunMaintenance(ctx, time.Now())
		}
	}
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return fmt.Sprintf("%d", t.Unix())
}

// handleMetrics exposes Prometheus text format. Ages are computed by Prometheus
// (time() - timestamp), so a frozen server is caught by up==0, not by stale numbers.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP holdfast_server_up 1 if the server is serving.\n# TYPE holdfast_server_up gauge\nholdfast_server_up 1\n")
	b.WriteString("# HELP holdfast_agent_last_seen_timestamp_seconds Last poll or report from the agent.\n# TYPE holdfast_agent_last_seen_timestamp_seconds gauge\n")
	names := make([]string, 0, len(s.specs))
	for n := range s.specs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "holdfast_agent_last_seen_timestamp_seconds{host=%q} %s\n", n, ts(s.host(n).LastSeen))
	}
	b.WriteString("# HELP holdfast_job_last_success_timestamp_seconds Finish time of the last successful or warning backup.\n# TYPE holdfast_job_last_success_timestamp_seconds gauge\n")
	for _, n := range names {
		for _, j := range s.specs[n].Jobs {
			fmt.Fprintf(&b, "holdfast_job_last_success_timestamp_seconds{host=%q,job=%q} %s\n", n, j.ID, ts(s.host(n).Jobs[j.ID].LastSuccess))
		}
	}
	b.WriteString("# HELP holdfast_job_expected_interval_seconds Configured interval; alerts fire at a multiple of this.\n# TYPE holdfast_job_expected_interval_seconds gauge\n")
	for _, n := range names {
		for _, j := range s.specs[n].Jobs {
			fmt.Fprintf(&b, "holdfast_job_expected_interval_seconds{host=%q,job=%q} %d\n", n, j.ID, int64(j.Interval.Seconds()))
		}
	}
	b.WriteString("# HELP holdfast_job_last_status 1 for the status of the most recent run.\n# TYPE holdfast_job_last_status gauge\n")
	for _, n := range names {
		for _, j := range s.specs[n].Jobs {
			st := s.host(n).Jobs[j.ID].LastStatus
			for _, cand := range []string{proto.StatusSuccess, proto.StatusWarning, proto.StatusFailed, proto.StatusInterrupted} {
				v := 0
				if st == cand {
					v = 1
				}
				fmt.Fprintf(&b, "holdfast_job_last_status{host=%q,job=%q,status=%q} %d\n", n, j.ID, cand, v)
			}
		}
	}
	b.WriteString("# HELP holdfast_runs_total Runs reported by agents.\n# TYPE holdfast_runs_total counter\n")
	for _, n := range names {
		keys := make([]string, 0)
		for k := range s.host(n).Runs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := strings.SplitN(k, "|", 3)
			fmt.Fprintf(&b, "holdfast_runs_total{host=%q,kind=%q,job=%q,status=%q} %d\n", n, p[0], p[1], p[2], s.host(n).Runs[k])
		}
	}
	b.WriteString("# HELP holdfast_maintenance_last_success_timestamp_seconds Last successful forget+prune+check.\n# TYPE holdfast_maintenance_last_success_timestamp_seconds gauge\n")
	for _, n := range names {
		fmt.Fprintf(&b, "holdfast_maintenance_last_success_timestamp_seconds{host=%q} %s\n", n, ts(s.host(n).Maint.LastSuccess))
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

// EscrowExport writes every repository password to path (0600) for offline storage.
func (s *Server) EscrowExport(path string) error {
	s.mu.Lock()
	out := map[string]map[string]string{}
	for name, hs := range s.st.Hosts {
		if hs.Password != "" {
			out[name] = map[string]string{"repository": s.RepoFor(name), "password": hs.Password}
		}
	}
	s.mu.Unlock()
	b, _ := json.MarshalIndent(out, "", "  ")
	return os.WriteFile(path, b, 0o600)
}
