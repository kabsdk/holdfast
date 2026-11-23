package server

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"github.com/kabsdk/holdfast/internal/pin"
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

// newTestServer builds a server on a fresh database. Its storage always passes. now, if given, is its clock.
func newTestServer(t *testing.T, now ...func() time.Time) *Server {
	return newTestServerWith(t, func(d *Deps) {
		if len(now) > 0 {
			d.Now = now[0]
		}
	})
}

// newTestServerWith builds a server on a fresh database after opt has changed its dependencies.
func newTestServerWith(t *testing.T, opt func(*Deps)) *Server {
	t.Helper()
	sealer, _ := creds.NewSealer(creds.Random(creds.KeySize))
	set := Settings{PublicURL: "https://holdfast.example.test", StateDir: t.TempDir(), ResticBin: fakeRestic(t), DefaultZone: "Europe/Copenhagen", NodeName: "test",
		Storage: StorageSettings{Endpoint: "http://127.0.0.1:9000", Bucket: "holdfast"}, Maintenance: MaintenanceSettings{Check: CheckDaily, CheckCycleDays: 30}}
	d := Deps{Store: testpg.Store(t), Sealer: sealer, Prober: storage.StaticProber{},
		Maintenance: storage.Key{AccessKeyID: "MAINT", SecretAccessKey: "maint-secret"}, Gateway: storage.Key{AccessKeyID: "GATEWAY", SecretAccessKey: "gateway-secret"},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	opt(&d)
	s, err := New(set, d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func job(paths ...string) *config.Job {
	return &config.Job{Paths: paths, Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}},
		Retention: config.Retention{Daily: &config.Duration{N: 14, Unit: config.Days}, Monthly: &config.Duration{N: 12, Unit: config.Months}, KeepLast: 3}}
}

var root = Actor{Name: "local:root", Source: "cli", Everywhere: true}

// certOf stands in for the fingerprint of the certificate a host's agent enrolls with.
func certOf(fqdn string) string { return creds.HashToken("the certificate of " + fqdn) }

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
	if sc := res.Enrollment.InstallScript; !strings.HasPrefix(sc, "curl -sfL ") || !strings.HasSuffix(sc, " sh -") ||
		!strings.Contains(sc, "HOLDFAST_FQDN=web01.example.com HOLDFAST_CODE="+res.Enrollment.Code) {
		t.Fatalf("install script: %s", sc)
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

	// Enrollment: a wrong code is refused (and audited); the right one pins the agent's certificate, once.
	cert := certOf("web01.example.com")
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: "WRONG-CODE-0000"}, cert); err == nil {
		t.Fatal("wrong code accepted")
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: strings.ToLower(res.Enrollment.Code)}, ""); err == nil {
		t.Fatal("enrolled without a certificate")
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: strings.ToLower(res.Enrollment.Code), Facts: proto.Facts{HostnameFQDN: "web01"}}, cert); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: res.Enrollment.Code}, certOf("another")); err == nil {
		t.Fatal("a code was used twice")
	}
	fqdn, err := s.st.Q().AgentByCertificate(ctx, cert)
	if err != nil || fqdn != "web01.example.com" {
		t.Fatalf("certificate lookup: %q %v", fqdn, err)
	}
	if _, err := s.st.Q().AgentByCertificate(ctx, certOf("another")); err == nil {
		t.Fatal("a certificate nobody enrolled with is a host")
	}

	// First poll: config and credentials, at once.
	p, err := s.Poll(ctx, fqdn, proto.PollRequest{}, 0)
	if err != nil || p.Config == nil || p.Credentials == nil || p.Credentials.RepoPassword == "" {
		t.Fatalf("first poll: %+v %v", p, err)
	}
	if p.Config.Revision != 1 || p.Config.Jobs[0].Schedule.Zone != "Europe/Copenhagen" || p.Config.Repository != "rest:https://holdfast.example.test/gateway/v1/web01.example.com/" {
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
	if err != nil || rev.Revision != 2 {
		t.Fatalf("set job: %+v %v", rev, err)
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
		`holdfast_runs_total{host="web01.example.com",kind="backup",job_id="system",status="success"} 1`, `holdfast_job_longest_gap_seconds{host="web01.example.com",job_id="system"} 86400`} {
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
		if strings.Contains(blob, pw) || strings.Contains(blob, "gateway-secret") {
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
	// The agent listener: TLS that asks each client for its certificate (decision 041).
	ts := httptest.NewUnstartedServer(s.AgentHandler())
	ts.TLS = AgentTLSConfig()
	ts.StartTLS()
	defer ts.Close()
	// with returns a client that pins the server and, given an identity, connects with its certificate.
	with := func(identity []byte) (*http.Client, string) {
		t.Helper()
		var cert *tls.Certificate
		fp := ""
		if identity != nil {
			c, err := pin.ParseIdentity(identity)
			if err != nil {
				t.Fatal(err)
			}
			cert, fp = c, pin.Fingerprint(c.Leaf)
		}
		c, err := pin.Client(ts.URL, pin.Fingerprint(ts.Certificate()), cert)
		if err != nil {
			t.Fatal(err)
		}
		return c, fp
	}
	post := func(c *http.Client, path string, body any, out any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := c.Post(ts.URL+proto.Base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(out)
		return resp.StatusCode
	}
	mine, _ := pin.NewIdentity("db01.example.com")
	other, _ := pin.NewIdentity("db01.example.com") // the same name, another key
	anon, _ := with(nil)
	agent, fp := with(mine)
	stranger, _ := with(other)
	var pr proto.PollResponse
	var er proto.EnrollResponse
	enroll := proto.EnrollRequest{FQDN: "db01.example.com", Code: res.Enrollment.Code}

	// Without a certificate nothing works, not even with the right code; /metrics and /healthz need none.
	if code := post(anon, "/poll?wait=0s", struct{}{}, &pr); code != http.StatusUnauthorized {
		t.Fatalf("a poll without a certificate: %d", code)
	}
	if code := post(anon, "/enroll", enroll, &er); code != http.StatusBadRequest {
		t.Fatalf("enrollment without a certificate: %d", code)
	}
	if resp, err := anon.Get(ts.URL + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("/healthz without a certificate: %v %v", resp, err)
	}
	// A certificate nobody enrolled with is no host.
	if code := post(agent, "/poll?wait=0s", struct{}{}, &pr); code != http.StatusUnauthorized {
		t.Fatalf("a poll before enrollment: %d", code)
	}
	// Enrollment pins the certificate the connection was made with.
	if code := post(agent, "/enroll", enroll, &er); code != 200 || er.CertificateSHA256 != fp {
		t.Fatalf("enroll over TLS: %d, pinned %q, want %q", code, er.CertificateSHA256, fp)
	}
	if code := post(agent, "/poll?wait=0s", struct{}{}, &pr); code != 200 || pr.Config == nil || pr.Config.FQDN != "db01.example.com" {
		t.Fatalf("a poll with the pinned certificate: %d %+v", code, pr.Config)
	}
	// Another key is not the host, whatever name its certificate carries.
	if code := post(stranger, "/poll?wait=0s", struct{}{}, &pr); code != http.StatusUnauthorized {
		t.Fatalf("a poll with another certificate of the same name: %d", code)
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
	resp, err := client.Get("http://holdfast/api/v1/status")
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
