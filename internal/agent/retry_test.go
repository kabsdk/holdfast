package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/testpg"
)

func TestPlanRetry(t *testing.T) {
	a := &Agent{}
	daily := proto.JobConfig{ID: "j", Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}, Zone: "UTC"}}
	slot := time.Date(2026, 11, 22, 2, 0, 0, 0, time.UTC)
	end := slot.Add(time.Minute)
	st := &jobState{LastSlot: slot, LastStatus: proto.StatusFailed, retryable: true}
	for i, want := range RetryAfter {
		st.Retries = i
		a.planRetry(daily, st, end)
		if !st.RetryAt.Equal(end.Add(want)) {
			t.Fatalf("retry %d: at %v, want %v", i+1, st.RetryAt, end.Add(want))
		}
	}
	st.Retries = len(RetryAfter)
	if a.planRetry(daily, st, end); !st.RetryAt.IsZero() || st.Retries != 0 {
		t.Fatalf("after the last retry, no more: %+v", st)
	}
	st.Retries, st.retryable = 0, false
	if a.planRetry(daily, st, end); !st.RetryAt.IsZero() {
		t.Fatal("a run stopped at its timeout is not retried")
	}
	every := proto.JobConfig{ID: "j", Schedule: schedule.Schedule{Every: &schedule.Every{N: 10, Unit: "minutes", From: "00:00"}, Zone: "UTC"}}
	st = &jobState{LastSlot: slot, LastStatus: proto.StatusFailed, retryable: true}
	if a.planRetry(every, st, slot.Add(6*time.Minute)); !st.RetryAt.IsZero() {
		t.Fatal("no retry when the next slot comes first: it runs the job anyway")
	}
	st = &jobState{LastSlot: slot, LastStatus: proto.StatusSuccess, Retries: 2, RetryAt: end}
	if a.planRetry(daily, st, end); !st.RetryAt.IsZero() || st.Retries != 0 {
		t.Fatal("a success ends the retries")
	}
}

// A real agent against an in-process server, with a restic that fails its first backup: the retry runs
// before the next slot, reports itself as one, and succeeds. The upload limit reaches restic.
func TestRetryAndUploadLimit(t *testing.T) {
	saved := RetryAfter
	RetryAfter = []time.Duration{time.Second}
	defer func() { RetryAfter = saved }()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	log := filepath.Join(dir, "args")
	script := `#!/bin/sh
echo "$*" >> ` + log + `
case "$*" in
  version) echo "restic 0.19.1 compiled with go1.24 on linux/amd64" ;;
  "cat config") exit 10 ;;
  *backup*)
    if [ ! -f ` + dir + `/failed-once ]; then touch ` + dir + `/failed-once; echo "Fatal: unable to open repository" >&2; exit 1; fi
    echo "{\"message_type\":\"summary\",\"snapshot_id\":\"$(date +%s%N)abcdef\",\"files_new\":1,\"data_added\":10}" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
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
	job := &config.Job{Paths: []string{dir}, Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}}, LimitUploadKiB: 512,
		Retention: config.Retention{Daily: &config.Duration{N: 14, Unit: config.Days}, Monthly: &config.Duration{N: 12, Unit: config.Months}, KeepLast: 3}}
	res, err := srv.AddHost(ctx, root, server.AddHostInput{FQDN: "web01.example.com", JobID: "system", Job: job})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: url, CASHA256: pinned, FQDN: "web01.example.com", StateDir: filepath.Join(dir, "agent"), CredentialsDir: filepath.Join(dir, "creds"),
		ResticBin: bin, PollTimeout: time.Second, Tick: 100 * time.Millisecond}
	if err := Enroll(ctx, cfg, res.Enrollment.Code, quiet); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()
	waitFor(t, "a failed backup, then a retry that succeeds", 20*time.Second, func() bool {
		evs, _ := srv.HostEvents(ctx, "web01.example.com", 10)
		var failed, retried bool
		for _, e := range evs {
			failed = failed || (e.Kind == "backup" && e.Status == "failed")
			retried = retried || (e.Kind == "backup" && e.Status == "success" && e.Detail["retry"] == "1 of 1")
		}
		return failed && retried
	})
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--limit-upload 512") {
		t.Fatalf("the upload limit must reach restic:\n%s", b)
	}
}
