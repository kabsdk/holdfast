package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/pin"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/testpg"
)

// fakeRestic answers like restic: a JSON summary with a fresh snapshot ID for backup, a version,
// and success for everything else except "cat config" (no repository yet). The e2e tests use the
// real restic.
const fakeRestic = `#!/bin/sh
case "$*" in
  version) echo "restic 0.19.1 compiled with go1.24 on linux/amd64" ;;
  "cat config") exit 10 ;;
  *backup*) echo "{\"message_type\":\"summary\",\"snapshot_id\":\"$(date +%s%N)abcdef\",\"files_new\":1,\"data_added\":10}" ;;
esac
exit 0
`

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", d, what)
}

// agentListener serves h as holdfast-server's agent listener does: over TLS, asking each client for its certificate
// (decision 041). It returns the URL and the fingerprint an agent pins.
func agentListener(t *testing.T, h http.Handler) (url, pinned string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = server.AgentTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts.URL, pin.Fingerprint(ts.Certificate())
}

func TestAgentAgainstServer(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte(fakeRestic), 0o755); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	sealer, _ := creds.NewSealer(creds.Random(creds.KeySize))
	srv, err := server.New(server.Settings{PublicURL: "http://test", StateDir: filepath.Join(dir, "server"), ResticBin: bin, DefaultZone: "UTC", NodeName: "t",
		Storage: server.StorageSettings{Endpoint: "http://127.0.0.1:1", Bucket: "b"}, Maintenance: server.MaintenanceSettings{CheckCycleDays: 30}},
		server.Deps{Store: testpg.Store(t), Sealer: sealer, Prober: storage.StaticProber{}, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srv.Run(ctx)
	url, pinned := agentListener(t, srv.AgentHandler())

	root := server.Actor{Name: "local:root", Source: "cli", Everywhere: true}
	job := &config.Job{Paths: []string{dir}, Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}},
		Retention: config.Retention{Daily: &config.Duration{N: 14, Unit: config.Days}, Monthly: &config.Duration{N: 12, Unit: config.Months}, KeepLast: 3}}
	res, err := srv.AddHost(ctx, root, server.AddHostInput{FQDN: "web01.example.com", JobID: "system", Job: job})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: url, CASHA256: pinned, FQDN: "web01.example.com", StateDir: filepath.Join(dir, "agent"), CredentialsDir: filepath.Join(dir, "creds"),
		ResticBin: bin, PollTimeout: time.Second, Tick: 100 * time.Millisecond}
	// Before enrollment the agent has no identity, and cannot run.
	if a, err := New(cfg, quiet); err != nil || a.Run(ctx) == nil {
		t.Fatalf("an agent that never enrolled must refuse to run: %v", err)
	}
	if err := Enroll(ctx, cfg, res.Enrollment.Code, quiet); err != nil {
		t.Fatal(err)
	}
	// The identity is the agent's own key and certificate, root-only, and no token is kept.
	if st, err := os.Stat(filepath.Join(cfg.StateDir, identityFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the agent's identity file: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "token")); err == nil {
		t.Fatal("an agent token was written; agents are known by their certificate")
	}
	a, err := New(cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	// The first backup runs right after enrollment, without waiting for 02:00.
	waitFor(t, "first backup reported", 15*time.Second, func() bool {
		h, err := srv.Host(ctx, "web01.example.com")
		return err == nil && h.Progress == "first_backup_done"
	})
	h, _ := srv.Host(ctx, "web01.example.com")
	if !h.InSync || h.Agent == nil || h.Agent.ResticVersion != "0.19.1" {
		t.Fatalf("host after first backup: in_sync=%v agent=%+v", h.InSync, h.Agent)
	}
	if st, err := os.Stat(filepath.Join(cfg.CredentialsDir, "repo.pass")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("repository password file: %v %v", st, err)
	}

	// Back up now: delivered, run once, reported with its command ID.
	cmdID, err := srv.RunNow(ctx, root, "web01.example.com", "system")
	if err != nil {
		t.Fatal(err)
	}
	countFor := func(id string) int {
		evs, _ := srv.HostEvents(ctx, "web01.example.com", 100)
		n := 0
		for _, e := range evs {
			if e.CommandID == id {
				n++
			}
		}
		return n
	}
	waitFor(t, "backup-now reported", 15*time.Second, func() bool { return countFor(cmdID) == 1 })
	time.Sleep(2 * time.Second) // more polls: the acknowledged command must not run again
	if n := countFor(cmdID); n != 1 {
		t.Fatalf("backup-now ran %d times, want 1", n)
	}

	// A config change reaches the agent, and the host is in sync again once it is applied.
	if _, err := srv.SetJob(ctx, root, "web01.example.com", "system", server.SetJobInput{Message: "two paths", Job: config.Job{
		Paths: []string{dir, "/tmp"}, Schedule: job.Schedule, Retention: job.Retention}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new revision applied", 15*time.Second, func() bool {
		h, _ := srv.Host(ctx, "web01.example.com")
		return h.InSync && h.Agent != nil && h.Agent.AppliedRevision == 2
	})
}
