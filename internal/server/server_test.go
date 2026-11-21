package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/testpg"
)

// fakeRestic stands in for restic during storage set-up: "cat config" fails (no repository yet)
// and every other command succeeds.
func fakeRestic(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n[ \"$1\" = cat ] && exit 10\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	sealer, _ := creds.NewSealer(creds.Random(creds.KeySize))
	set := Settings{PublicURL: "https://holdfast.example.test", StateDir: t.TempDir(), ResticBin: fakeRestic(t), DefaultZone: "Europe/Copenhagen", NodeName: "test",
		Storage: StorageSettings{Endpoint: "http://127.0.0.1:9000", Bucket: "holdfast"}, Maintenance: MaintenanceSettings{CheckCycleDays: 30}}
	s, err := New(set, Deps{Store: testpg.Store(t), Sealer: sealer, Provisioner: storage.NoopProvisioner{},
		Maintenance: storage.Key{AccessKeyID: "MAINT", SecretAccessKey: "maint-secret"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func job(paths ...string) *config.Job {
	return &config.Job{Paths: paths, Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}},
		Retention: config.Retention{Daily: &config.Duration{N: 14, Unit: config.Days}, KeepLast: 3}}
}

var root = Actor{Name: "local:root", Source: "cli"}

func TestHostLifecycle(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go s.hub.listen(runCtx, s.st.Pool(), s.log)

	// Add host with its first job: revision 1, credentials, a 15-minute code.
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: "web01.example.com", JobID: "system", Job: job("/etc")})
	if err != nil || res.Revision != 1 || res.Warning != "" {
		t.Fatalf("add host: %+v %v", res, err)
	}
	if !strings.Contains(res.Enrollment.InstallCommand, "--fqdn web01.example.com --code "+res.Enrollment.Code) {
		t.Fatalf("install command: %s", res.Enrollment.InstallCommand)
	}
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: "web01.example.com"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate host: %v", err)
	}
	var ve *ValidationError
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: "Web01"}); !errors.As(err, &ve) {
		t.Fatalf("invalid FQDN: %v", err)
	}
	h, _ := s.Host(ctx, "web01.example.com")
	if h.Progress != "waiting" || h.State != "awaiting_enrollment" {
		t.Fatalf("before enrollment: %+v", h)
	}

	// Enrollment: a wrong code is refused (and audited); the right one gives a token, once.
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: "WRONG-CODE-0000"}); err == nil {
		t.Fatal("wrong code accepted")
	}
	tok, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: strings.ToLower(res.Enrollment.Code), Facts: proto.Facts{HostnameFQDN: "web01"}})
	if err != nil || tok == "" {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: res.Enrollment.Code}); err == nil {
		t.Fatal("a code was used twice")
	}
	fqdn, err := s.st.Q().AgentByToken(ctx, creds.HashToken(tok))
	if err != nil || fqdn != "web01.example.com" {
		t.Fatalf("token lookup: %q %v", fqdn, err)
	}

	// First poll: config and credentials, at once.
	p, err := s.Poll(ctx, fqdn, proto.PollRequest{}, 0)
	if err != nil || p.Config == nil || p.Credentials == nil || p.Credentials.RepoPassword == "" {
		t.Fatalf("first poll: %+v %v", p, err)
	}
	if p.Config.Revision != 1 || p.Config.Jobs[0].Schedule.Zone != "Europe/Copenhagen" || p.Config.Repository != "s3:http://127.0.0.1:9000/holdfast/repos/web01.example.com" {
		t.Fatalf("rendered config: %+v", p.Config)
	}
	applied := proto.Applied{Revision: p.Config.Revision, SHA256: p.Config.SHA256}

	// Nothing new: the poll waits for its timeout.
	start := time.Now()
	p2, err := s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: p.Credentials.Version}, 300*time.Millisecond)
	if err != nil || p2.Config != nil || p2.Credentials != nil || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("idle poll: %+v %v after %v", p2, err, time.Since(start))
	}
	h, _ = s.Host(ctx, fqdn)
	if !h.InSync || !h.Online || h.Progress != "connected" {
		t.Fatalf("after first poll: in_sync=%v online=%v progress=%s", h.InSync, h.Online, h.Progress)
	}

	// A change wakes a waiting poll at once (LISTEN/NOTIFY), and the host shows drift until applied.
	done := make(chan proto.PollResponse, 1)
	go func() {
		r, _ := s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: 1}, 10*time.Second)
		done <- r
	}()
	time.Sleep(200 * time.Millisecond)
	base := int64(1)
	rev, err := s.SetJob(ctx, root, fqdn, "www", SetJobInput{Base: &base, Message: "add www", Job: *job("/var/www")})
	if err != nil || rev != 2 {
		t.Fatalf("set job: %d %v", rev, err)
	}
	select {
	case r := <-done:
		if r.Config == nil || len(r.Config.Jobs) != 2 || r.Config.Revision != 2 {
			t.Fatalf("woken poll: %+v", r.Config)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the change did not wake the waiting poll")
	}
	h, _ = s.Host(ctx, fqdn)
	if h.InSync {
		t.Fatal("the host should show drift until the agent applies revision 2")
	}
	if _, err := s.SetJob(ctx, root, fqdn, "www", SetJobInput{Base: &base, Job: *job("/srv")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a save from a stale base must be refused: %v", err)
	}

	// Back up now: the command is delivered until acknowledged.
	cmdID, err := s.RunNow(ctx, root, fqdn, "system")
	if err != nil {
		t.Fatal(err)
	}
	p3, _ := s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: 1}, 0)
	if len(p3.Commands) != 1 || p3.Commands[0].ID != cmdID {
		t.Fatalf("command not delivered: %+v", p3.Commands)
	}
	p4, _ := s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: 1, AckedCommands: []string{cmdID}}, 0)
	if len(p4.Commands) != 0 {
		t.Fatalf("acknowledged command delivered again: %+v", p4.Commands)
	}

	// Events: stored once however often they are sent; the first success completes enrollment progress.
	now := time.Now()
	ev := proto.Event{ID: "ev-1", Kind: proto.KindBackup, JobID: "system", Status: proto.StatusSuccess, Started: now, Finished: now, SnapshotID: "abc"}
	for i := 0; i < 2; i++ {
		acked, err := s.Events(ctx, fqdn, []proto.Event{ev})
		if err != nil || len(acked) != 1 {
			t.Fatalf("events: %v %v", acked, err)
		}
	}
	h, _ = s.Host(ctx, fqdn)
	if h.Progress != "first_backup_done" || h.Jobs[0].Status == nil || h.Jobs[0].Status.LastSnapshotID != "abc" {
		t.Fatalf("after first backup: progress=%s jobs=%+v", h.Progress, h.Jobs)
	}

	// Diff and metrics.
	changes, err := s.Diff(ctx, 1, 2)
	if err != nil || len(changes) == 0 || !strings.HasPrefix(changes[0].Path, "hosts.web01.example.com.jobs.www") {
		t.Fatalf("diff: %+v %v", changes, err)
	}
	rr := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{"holdfast_config_head_revision 2", `holdfast_agent_config_in_sync{host="web01.example.com"} 0`,
		`holdfast_runs_total{host="web01.example.com",kind="backup",job="system",status="success"} 1`, `holdfast_job_longest_gap_seconds{host="web01.example.com",job="system"} 86400`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	// Credentials never appear in revisions, the audit log or the metrics.
	c, _ := s.st.Q().GetCredentials(ctx, fqdn)
	pw, _ := s.seal.Open(c.RepoPassword, fqdn)
	r1, _ := s.st.Q().GetRevision(ctx, 2)
	b, _ := r1.Config.Canonical()
	audit, _ := s.st.Q().ListAudit(ctx, "", 100)
	ab, _ := json.Marshal(audit)
	for name, blob := range map[string]string{"revision": string(b), "audit": string(ab), "metrics": rr.Body.String()} {
		if strings.Contains(blob, pw) || strings.Contains(blob, p.Credentials.S3SecretAccessKey) || strings.Contains(blob, tok) {
			t.Errorf("a credential leaked into the %s", name)
		}
	}
}

func TestAgentAuthAndAPIOverSocket(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: "db01.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.AgentHandler())
	defer ts.Close()
	// Polling without a token is refused.
	resp, _ := http.Post(ts.URL+proto.Base+"/poll", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated poll: %d", resp.StatusCode)
	}
	// Enroll over HTTP, then poll with the token.
	body, _ := json.Marshal(proto.EnrollRequest{FQDN: "db01.example.com", Code: res.Enrollment.Code})
	resp, _ = http.Post(ts.URL+proto.Base+"/enroll", "application/json", bytes.NewReader(body))
	var er proto.EnrollResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if resp.StatusCode != 200 || er.Token == "" {
		t.Fatalf("enroll over HTTP: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", ts.URL+proto.Base+"/poll?wait=0s", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+er.Token)
	resp, _ = http.DefaultClient.Do(req)
	var pr proto.PollResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	if resp.StatusCode != 200 || pr.Config == nil || pr.Config.FQDN != "db01.example.com" {
		t.Fatalf("authenticated poll: %d %+v", resp.StatusCode, pr.Config)
	}

	// The API over a Unix socket records the peer's user as the actor.
	sock := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.APIHandler(true), ConnContext: PeerActor()}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err = client.Get("http://holdfast/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	me, _ := user.Current()
	if st["actor"] != "local:"+me.Username || st["head_revision"].(float64) != 1 {
		t.Fatalf("status over socket: %+v", st)
	}
	// Validation errors are 422 with the problems listed.
	resp, _ = client.Post("http://holdfast/api/v1/hosts", "application/json", strings.NewReader(`{"fqdn":"not_valid"}`))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid host over the API: %d", resp.StatusCode)
	}
}
