package e2e

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kabsdk/holdfast/internal/pin"
)

func smallSource(t *testing.T) string {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644))
	return src
}

func countStatus(evs []event, job, status string) int {
	n := 0
	for _, e := range evs {
		if e.JobID == job && e.Status == status {
			n++
		}
	}
	return n
}

// SIGKILL the agent mid-upload: the next start reports "interrupted" (never silence), removes the
// stale lock, picks up the revision saved while it was down, and backs up successfully (PoC test 3).
// Upload is slowed by a throttling proxy in front of S3 (plan-m1 Q11: no product option for it).
func TestKilledBackupIsReportedAndRecovered(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big")
	must(t, os.MkdirAll(src, 0o755))
	buf := make([]byte, 24<<20)
	_, _ = rand.Read(buf)
	must(t, os.WriteFile(filepath.Join(src, "random.bin"), buf, 0o644))

	u, _ := url.Parse(s3Endpoint)
	proxy := newThrottle(t, u.Host)
	e := newEnvWith(t, envOpts{S3Endpoint: "http://" + proxy.addr()})
	const fqdn = "app01.example.com"
	code := e.addHost(fqdn, "big", "--path", src, "--daily", "02:00")
	proxy.rate.Store(128 << 10) // 128 KiB/s: 24 MiB of random data takes minutes
	a := e.enrollAgent(fqdn, code)
	e.runAgent(a)
	marker := filepath.Join(a.dir, "state", "running.json")
	waitFor(t, "backup in progress", time.Minute, func() bool { _, err := os.Stat(marker); return err == nil })
	time.Sleep(3 * time.Second)
	a.kill(syscall.SIGKILL) // the agent and restic die together, mid-upload
	countLocks := func() int {
		return strings.Count(mc(t, "ls", "--recursive", "hf/"+e.bucket+"/repos/"+fqdn+"/"), "locks/")
	}
	locks := countLocks()

	// While the agent is down, someone changes the job: a new revision it must pick up on start.
	e.cli("job", "set", fqdn, "big", "--path", src, "--daily", "02:00", "--exclude", "*.tmp", "-m", "exclude temporary files", testAccept)
	proxy.rate.Store(0)
	e.runAgent(a)
	waitFor(t, "interrupted reported, then a success on the new revision", 3*time.Minute, func() bool {
		h := e.host(fqdn)
		evs := e.events(fqdn)
		return countStatus(evs, "big", "interrupted") == 1 && countStatus(evs, "big", "success") >= 1 && h.InSync && h.Agent != nil && h.Agent.AppliedRevision == 3
	})
	evs := e.events(fqdn) // newest first: the success came after the interruption
	var order []string
	for i := len(evs) - 1; i >= 0; i-- {
		order = append(order, evs[i].Status)
	}
	if order[0] != "interrupted" {
		t.Fatalf("the first reported run should be the interrupted one, got %v", order)
	}
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_runs_total{host="app01.example.com",kind="backup",job_id="big",status="interrupted"}`] != 1 {
		t.Fatal("the interrupted run is not counted in the metrics")
	}
	if left := countLocks(); left != 0 {
		t.Fatalf("%d locks left in the repository after recovery", left)
	}
	t.Logf("locks left by SIGKILL: %d; reported sequence: %v", locks, order)
}

// The answers to event deliveries are lost three times: the agent sends again, the server stores
// each event once. The scheduled runs land on their minute slots.
func TestEventsExactlyOnceAndScheduleSlots(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "web02.example.com"
	code := e.addHost(fqdn, "etc", "--path", src, "--every", "1m")
	a := e.enrollAgent(fqdn, code)
	proxy := newDropProxy(t, e, a, 3)
	pointAgentAt(t, a, e.url, proxy.url)
	e.runAgent(a)
	key := `holdfast_runs_total{host="web02.example.com",kind="backup",job_id="etc",status="success"}`
	waitFor(t, "three answers dropped, then three backups delivered", 4*time.Minute, func() bool {
		return proxy.dropped.Load() == 3 && a.spooled() == 0 && metrics(t, e.url+"/metrics")[key] >= 3
	})
	r := e.resticFor(e.escrow()[fqdn])
	total := snapshots(t, r)
	waitFor(t, "every snapshot counted once", 30*time.Second, func() bool {
		return a.spooled() == 0 && metrics(t, e.url+"/metrics")[key] == float64(snapshots(t, r))
	})
	evs := e.events(fqdn)
	if n := countStatus(evs, "etc", "success"); n < total {
		t.Fatalf("%d success events for %d snapshots", n, total)
	}
	seen := map[string]bool{}
	slotted := 0
	for _, ev := range evs {
		if seen[ev.ID] {
			t.Fatalf("event %s stored twice", ev.ID)
		}
		seen[ev.ID] = true
		if ev.ScheduledFor == nil || ev.Status != "success" {
			continue // the first backup runs at enrollment, without a slot
		}
		slotted++
		if ev.ScheduledFor.Second() != 0 {
			t.Errorf("scheduled for %v: not on a minute slot", ev.ScheduledFor)
		}
		if late := ev.Started.Sub(*ev.ScheduledFor); late < 0 || late > 5*time.Second {
			t.Errorf("slot %v started %v late", ev.ScheduledFor.Format("15:04:05"), late)
		}
	}
	if slotted < 2 {
		t.Fatalf("only %d scheduled runs seen", slotted)
	}
	t.Logf("%d answers dropped; %d snapshots, %d success events, each stored once; %d runs on their slots", proxy.dropped.Load(), total, countStatus(evs, "etc", "success"), slotted)
}

// Config history: linear revisions, diffs, a stale base refused; drift while the agent is down; a
// repository check; the audit trail; and no credential anywhere a person or Prometheus can read.
func TestConfigHistoryDriftCheckAndNoLeaks(t *testing.T) {
	src := smallSource(t)
	other := smallSource(t)
	e := newEnv(t)
	const fqdn = "mon01.example.com"
	code := e.addHost(fqdn, "system", "--path", src, "--daily", "02:00", "-m", "add mon01")
	a := e.startAgent(fqdn, code)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	waitFor(t, "in sync", 30*time.Second, func() bool { return e.host(fqdn).InSync })

	// Revision 1 added the host and revision 2 its job (plan-m10 item 4); this is revision 3.
	e.cli("job", "set", fqdn, "system", "--path", src, "--path", other, "--daily", "02:00", "--base", "2", "-m", "two paths", testAccept)
	if out, err := e.cliErr("job", "set", fqdn, "system", "--path", src, "--daily", "03:00", "--base", "2", "-m", "stale", testAccept); err == nil || !strings.Contains(string(out), "conflict") {
		t.Fatalf("a save from a stale base must be refused with a conflict: %v %s", err, out)
	}
	var diff struct {
		Changes []struct {
			Path string `json:"path"`
			To   any    `json:"to"`
		} `json:"changes"`
	}
	must(t, json.Unmarshal(e.cli("revision", "diff", "2", "3"), &diff))
	if len(diff.Changes) != 1 || diff.Changes[0].Path != "hosts.mon01.example.com.jobs.system.paths[1]" || diff.Changes[0].To != other {
		t.Fatalf("diff 2 3: %+v", diff.Changes)
	}
	var revs []struct{ Number int64 }
	must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
	if len(revs) != 3 {
		t.Fatalf("revisions: %+v", revs)
	}
	waitFor(t, "revision 3 applied", 30*time.Second, func() bool { h := e.host(fqdn); return h.InSync && h.Agent != nil && h.Agent.AppliedRevision == 3 })

	// Drift: the agent is down while a change is saved.
	a.kill(syscall.SIGTERM)
	e.cli("job", "set", fqdn, "system", "--path", src, "--path", other, "--daily", "02:00", "--exclude", "*.log", "-m", "exclude logs", testAccept)
	if h := e.host(fqdn); h.InSync || h.DesiredRevision != 4 {
		t.Fatalf("the host should show drift: in_sync=%v desired=%d", h.InSync, h.DesiredRevision)
	}
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_agent_config_in_sync{host="mon01.example.com"}`] != 0 {
		t.Fatal("drift not exported")
	}
	e.runAgent(a)
	waitFor(t, "back in sync on revision 4", 30*time.Second, func() bool { h := e.host(fqdn); return h.InSync && h.Agent != nil && h.Agent.AppliedRevision == 4 })

	// A repository check: the server tests the storage at once, and the agent runs restic check in its own
	// loop and reports it (decision 030).
	var checks []struct{ FQDN, Status, Command, Error string }
	must(t, json.Unmarshal(e.cli("check"), &checks))
	if len(checks) != 1 || checks[0].Status != "requested" || checks[0].Command == "" {
		t.Fatalf("check: %+v", checks)
	}
	waitFor(t, "the agent's check", time.Minute, func() bool {
		for _, ev := range e.events(fqdn) {
			if ev.Kind == "check" && ev.CommandID == checks[0].Command {
				if ev.Status != "success" {
					t.Fatalf("the agent's check: %+v", ev)
				}
				return true
			}
		}
		return false
	})
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_maintenance_last_success_timestamp_seconds{host="mon01.example.com",kind="check"}`] == 0 {
		t.Fatal("check success not exported")
	}

	// The audit trail names the person and covers every action.
	actions := map[string]string{}
	for _, en := range e.audit(fqdn) {
		actions[en.Action] = en.Actor
	}
	for _, want := range []string{"host.add", "enrollment.code_issued", "agent.enrolled", "job.change", "maintenance.check_requested"} {
		if _, ok := actions[want]; !ok {
			t.Errorf("audit lacks %s (has %v)", want, actions)
		}
	}
	if actions["host.add"] != "local:root" {
		t.Errorf("host.add actor = %q, want local:root (the socket peer)", actions["host.add"])
	}

	// No credential in anything a person or Prometheus reads: logs, CLI and API output, metrics.
	h := e.escrow()[fqdn]
	gw, err := os.ReadFile(filepath.Join(e.dir, "gateway.env"))
	must(t, err)
	_, gwSecret, _ := strings.Cut(string(gw), "AWS_SECRET_ACCESS_KEY=")
	secrets := map[string]string{"repository password": h.RepoPassword, "gateway key's secret": strings.TrimSpace(gwSecret), "agent's private key": agentKeyLine(t, a)}
	var blobs []string
	for _, args := range [][]string{{"host", "list"}, {"host", "show", fqdn}, {"host", "events", fqdn}, {"audit"}, {"revision", "list"},
		{"revision", "show", "1"}, {"revision", "show", "4"}, {"status"}} {
		blobs = append(blobs, string(e.cli(args...)))
	}
	for _, l := range e.logs {
		b, _ := os.ReadFile(l)
		blobs = append(blobs, string(b))
	}
	resp, err := e.client.Get(e.url + "/metrics")
	must(t, err)
	mb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	blobs = append(blobs, string(mb))
	for what, s := range secrets {
		if len(s) < 16 {
			t.Fatalf("%s looks wrong: %q", what, s)
		}
		for _, b := range blobs {
			if strings.Contains(b, s) {
				t.Errorf("the %s leaked into output a person or Prometheus can read", what)
			}
		}
	}
}

// agentKeyLine is the first line of the agent's private key, as its identity file holds it in PEM.
func agentKeyLine(t *testing.T, a *agentProc) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.dir, "state", "agent.pem"))
	must(t, err)
	_, key, ok := strings.Cut(string(b), "-----BEGIN PRIVATE KEY-----\n")
	if !ok {
		t.Fatalf("no private key in the agent's identity file")
	}
	line, _, _ := strings.Cut(key, "\n")
	return line
}

// Enrollment: bad and duplicate hosts are refused; a code expires after 15 minutes and is refused
// (and audited); a new code revokes the open one; a used code is refused.
func TestEnrollment(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	if out, err := e.cliErr("host", "add", "Not_A_Host"); err == nil || !strings.Contains(string(out), "lower case") {
		t.Fatalf("an invalid FQDN must be refused: %v %s", err, out)
	}
	const fqdn = "ci04.example.com"
	code := e.addHost(fqdn, "system", "--path", src, "--daily", "02:00")
	if out, err := e.cliErr("host", "add", fqdn); err == nil || !strings.Contains(string(out), "conflict") {
		t.Fatalf("a duplicate host must be refused: %v %s", err, out)
	}
	if p := e.host(fqdn).Progress; p != "waiting" {
		t.Fatalf("progress before enrollment = %q", p)
	}

	// The code expires (forced in the database rather than waiting 15 minutes).
	conn, err := pgx.Connect(context.Background(), e.dsn)
	must(t, err)
	_, err = conn.Exec(context.Background(), `UPDATE enrollment_codes SET expires_at = now() - interval '1 minute'`)
	must(t, err)
	_ = conn.Close(context.Background())
	if _, out, err := e.enrollAgentWith(fqdn, code, e.url); err == nil {
		t.Fatalf("an expired code was accepted: %s", out)
	}
	refused := false
	for _, en := range e.audit(fqdn) {
		if en.Action == "agent.enroll_refused" && strings.Contains(en.Detail["reason"].(string), "expired") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("the refused enrollment is not audited with its reason")
	}

	var fresh struct{ Code string }
	must(t, json.Unmarshal(e.cli("host", "enroll-code", fqdn), &fresh))
	a, out, err := e.enrollAgentWith(fqdn, fresh.Code, e.url)
	if err != nil {
		t.Fatalf("enroll with a new code: %v %s", err, out)
	}
	if _, out, err := e.enrollAgentWith(fqdn, fresh.Code, e.url); err == nil {
		t.Fatalf("a used code was accepted again: %s", out)
	}
	e.runAgent(a)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
}

// TLS: the install command carries the server CA's fingerprint. An agent with a wrong pin refuses to
// talk to the server, and the code stays valid. So does an agent talking to a man in the middle who
// presents the real CA's certificate next to his own (decision 030). With the right pin and the real
// server, it enrolls and backs up. The other way round, the server knows the agent only by the certificate
// it enrolled with (decision 041).
func TestTLSPinning(t *testing.T) {
	src := smallSource(t)
	cert := serverCert(t)
	e := newEnvWith(t, envOpts{TLS: cert})
	const fqdn = "sec01.example.com"
	var res struct {
		Enrollment struct {
			Code           string `json:"code"`
			InstallCommand string `json:"install_command"`
			InstallScript  string `json:"install_script"`
		} `json:"enrollment"`
	}
	must(t, json.Unmarshal(e.cli("host", "add", fqdn), &res))
	e.cli("job", "set", fqdn, "system", "--path", src, "--daily", "02:00", testAccept)
	if !strings.Contains(res.Enrollment.InstallCommand, "--ca-sha256 "+cert.pin) {
		t.Fatalf("the install command does not pin the server: %s", res.Enrollment.InstallCommand)
	}
	if !strings.Contains(res.Enrollment.InstallScript, "HOLDFAST_CA_SHA256="+cert.pin) {
		t.Fatalf("the one-line install does not pin the server: %s", res.Enrollment.InstallScript)
	}
	if _, out, err := e.enrollAgentWith(fqdn, res.Enrollment.Code, e.url, "--ca-sha256", strings.Repeat("ab", 32)); err == nil || !strings.Contains(string(out), "pinned fingerprint") {
		t.Fatalf("an agent with the wrong pin must refuse the server: %v %s", err, out)
	}
	if _, out, err := e.enrollAgentWith(fqdn, res.Enrollment.Code, impostor(t, cert), "--ca-sha256", cert.pin); err == nil || !strings.Contains(string(out), "does not chain to the pinned certificate") {
		t.Fatalf("an agent must refuse a man in the middle who shows the real CA's certificate: %v %s", err, out)
	}
	a, out, err := e.enrollAgentWith(fqdn, res.Enrollment.Code, e.url, "--ca-sha256", cert.pin)
	if err != nil {
		t.Fatalf("enroll with the right pin: %v %s", err, out)
	}
	e.runAgent(a)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)

	// The server knows the agent by the certificate it enrolled with (decision 041): no certificate, or another
	// one with the same name, gets nothing.
	poll := func(c *http.Client) int {
		t.Helper()
		resp, err := c.Post(e.url+"/agent/v1/poll?wait=0s", "application/json", strings.NewReader("{}"))
		must(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := poll(e.agentClient(a)); code != http.StatusOK {
		t.Fatalf("the enrolled agent's certificate: %d", code)
	}
	noCert, err := pin.Client(e.url, cert.pin, nil)
	must(t, err)
	if code := poll(noCert); code != http.StatusUnauthorized {
		t.Fatalf("a poll without a certificate: %d", code)
	}
	otherPEM, err := pin.NewIdentity(fqdn)
	must(t, err)
	other, err := pin.ParseIdentity(otherPEM)
	must(t, err)
	stranger, err := pin.Client(e.url, cert.pin, other)
	must(t, err)
	if code := poll(stranger); code != http.StatusUnauthorized {
		t.Fatalf("another certificate with the host's name: %d", code)
	}

	// Enrolling again makes a new key, and the old certificate stops working.
	oldClient := e.agentClient(a)
	a.kill(syscall.SIGTERM)
	var fresh struct{ Code string }
	must(t, json.Unmarshal(e.cli("host", "enroll-code", fqdn), &fresh))
	a = e.enrollAgent(fqdn, fresh.Code)
	if code := poll(oldClient); code != http.StatusUnauthorized {
		t.Fatalf("the certificate from before the new enrollment: %d", code)
	}
	if code := poll(e.agentClient(a)); code != http.StatusOK {
		t.Fatalf("the new certificate: %d", code)
	}
}
