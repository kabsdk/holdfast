package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/kabsdk/holdfast/internal/agent"
)

// No commands on hosts, a restore onto another host, and rolling a repository back.

// editAgent changes the agent's own config, as root on the host would, while it is stopped.
func editAgent(t *testing.T, a *agentProc, edit func(c *agent.Config)) {
	t.Helper()
	c, err := agent.LoadConfig(a.cfgPath)
	must(t, err)
	edit(&c)
	must(t, agent.WriteConfig(a.cfgPath, c))
}

// restoreWait runs holdfast restore … --wait --json and returns the agent's event.
func (e *env) restoreWait(args ...string) (restoreEv, error) {
	e.t.Helper()
	out, err := e.cliErr(append([]string{"restore"}, append(args, "--wait")...)...)
	var ev restoreEv
	if err != nil {
		return ev, fmt.Errorf("%v: %s", err, out)
	}
	return ev, json.Unmarshal(out, &ev)
}

type restoreEv struct {
	Status string            `json:"status"`
	Error  string            `json:"error"`
	Detail map[string]string `json:"detail"`
}

func (e *env) latestSnapshot(fqdn, job string) string {
	e.t.Helper()
	for _, ev := range e.events(fqdn) {
		if ev.JobID == job && ev.Kind == "backup" && ev.SnapshotID != "" {
			return ev.SnapshotID
		}
	}
	e.t.Fatalf("no snapshot of %s/%s", fqdn, job)
	return ""
}

func waitStatus(t *testing.T, e *env, fqdn, job, status string, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%s/%s %s", fqdn, job, status), 2*time.Minute, func() bool { return countStatus(e.events(fqdn), job, status) >= n })
}

// Decision 052: Holdfast runs no command on a host. job set has no --dump, and an agent.yaml from before 0.6.0 that
// still defines dumps loads, says they are ignored, and backs up its paths.
func TestNoCommandsOnHosts(t *testing.T) {
	e := newEnv(t)
	const fqdn = "db01.example.com"
	if out, err := e.cliErr("job", "set", fqdn, "pg", "--dump", "pg"); err == nil || !strings.Contains(string(out), "unknown flag") {
		t.Fatalf("job set --dump must be gone: %v %s", err, out)
	}
	a := e.enrollAgent(fqdn, e.addHost(fqdn, "etc", "--path", smallSource(t), "--daily", "02:00"))
	editAgent(t, a, func(c *agent.Config) {
		c.Dumps = map[string]any{"pg": map[string]any{"command": []string{"/usr/bin/touch", filepath.Join(e.dir, "ran")}}}
	})
	e.runAgent(a)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	waitFor(t, "the agent's warning", time.Minute, func() bool {
		b, _ := os.ReadFile(e.logFile(filepath.Base(a.dir)))
		return strings.Contains(string(b), "dumps is ignored")
	})
	if _, err := os.Stat(filepath.Join(e.dir, "ran")); err == nil {
		t.Fatal("the agent ran a command from agent.yaml")
	}
}

func TestRestoreOntoAnotherHost(t *testing.T) {
	src := t.TempDir()
	must(t, os.WriteFile(filepath.Join(src, "invoice.txt"), []byte("invoice 42\n"), 0o640))
	e := newEnv(t)
	const a, b = "app01.example.com", "spare01.example.com"
	e.startAgent(a, e.addHost(a, "data", "--path", src, "--daily", "02:00"))
	e.waitProgress(a, "first_backup_done", 2*time.Minute)
	snap := e.latestSnapshot(a, "data")

	bAgent := e.enrollAgent(b, e.addHost(b, "etc", "--path", smallSource(t), "--daily", "02:00"))
	restoreRoot := filepath.Join(e.dir, "restore-spare01")
	editAgent(t, bAgent, func(c *agent.Config) { c.Restore.Root = restoreRoot })
	e.runAgent(bAgent)
	e.waitProgress(b, "first_backup_done", 2*time.Minute)

	// B cannot read A's repository through the gateway: no restore lets it.
	gw := func() int {
		t.Helper()
		resp, err := e.agentClient(bAgent).Get(e.url + "/gateway/v1/" + a + "/config")
		must(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := gw(); code != http.StatusForbidden {
		t.Fatalf("B read A's repository before any restore: %d", code)
	}

	// The host's opt-in is checked first.
	if out, err := e.cliErr("restore", a, snap, "--to-host", b); err == nil || !strings.Contains(string(out), "allow_other_hosts") {
		t.Fatalf("a host that has not opted in: %v %s", err, out)
	}

	// B opts in.
	bAgent.kill(syscall.SIGTERM)
	editAgent(t, bAgent, func(c *agent.Config) { c.Restore.AllowOtherHosts = true })
	e.runAgent(bAgent)
	waitFor(t, "B's opt-in", time.Minute, func() bool {
		bounds, _ := e.hostRaw(b)["agent"].(map[string]any)["restore_bounds"].(map[string]any)
		return bounds != nil && bounds["allow_other_hosts"] == true
	})
	if out, err := e.cliErr("restore", a, snap, "--to-host", b, "--in-place"); err == nil {
		t.Fatalf("in place onto another host: %s", out)
	}
	ev, err := e.restoreWait(a, snap, "--to-host", b, "-m", "e2e: A is gone")
	if err != nil || ev.Status != "success" || ev.Detail["source_host"] != a || !strings.HasPrefix(ev.Detail["directory"], restoreRoot+"/") {
		t.Fatalf("restore onto B: %+v %v", ev, err)
	}
	got, err := os.ReadFile(filepath.Join(ev.Detail["directory"], src, "invoice.txt"))
	if err != nil || string(got) != "invoice 42\n" {
		t.Fatalf("the restored file: %q %v", got, err)
	}
	// B kept A's secrets only for that restore, and may not read A's repository any more.
	left, _ := filepath.Glob(filepath.Join(bAgent.dir, "state", "source-*"))
	if len(left) != 0 {
		t.Fatalf("B kept A's credentials: %v", left)
	}
	if code := gw(); code != http.StatusForbidden {
		t.Fatalf("B may still read A's repository after the restore: %d", code)
	}
	var actions []string
	for _, x := range e.audit(a) {
		actions = append(actions, x.Action)
	}
	if !slices.Contains(actions, "restore.requested_elsewhere") {
		t.Fatalf("A's history does not say its snapshot went elsewhere: %v", actions)
	}
}

type rollbackOut struct {
	ID                int64    `json:"id"`
	Status            string   `json:"status"`
	Error             string   `json:"error"`
	SnapshotsBack     []string `json:"snapshots_back"`
	SnapshotsSetAside []string `json:"snapshots_set_aside"`
	CheckPassed       *bool    `json:"check_passed"`
}

func (e *env) rollback(fqdn string, to time.Time) rollbackOut {
	e.t.Helper()
	var r rollbackOut
	out, err := e.cliErr("agent", "rollback", fqdn, "--to", to.UTC().Format(time.RFC3339), "--confirm", fqdn, "--wait", "-m", "e2e")
	if jerr := json.Unmarshal(out, &r); jerr != nil {
		e.t.Fatalf("rollback: %v %v\n%s", err, jerr, out)
	}
	return r
}

func TestRollback(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "web01.example.com"
	a := e.startAgent(fqdn, e.addHost(fqdn, "data", "--path", src, "--daily", "02:00"))
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	for i := 2; i <= 3; i++ {
		must(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o644))
		e.cli("job", "run", fqdn, "data")
		waitStatus(t, e, fqdn, "data", "success", i)
	}
	ids := e.snapshotIDs(fqdn)
	if len(ids) != 3 {
		t.Fatalf("snapshots: %v", ids)
	}
	time.Sleep(2 * time.Second)
	good := time.Now()
	time.Sleep(2 * time.Second)

	// Two snapshots deleted behind Holdfast's back, with the storage admin's key.
	c := s3(t)
	for _, id := range ids[:2] {
		must(t, c.RemoveObject(context.Background(), e.bucket, "repos/"+fqdn+"/snapshots/"+id, minio.RemoveObjectOptions{}))
	}
	e.snapshotIDs(fqdn) // refresh
	if d, _ := e.hostRaw(fqdn)["disappeared"].(float64); d != 2 {
		t.Fatalf("disappeared: %v", d)
	}

	// The preview brings them back and sets nothing aside.
	var p struct {
		Refused           string   `json:"refused"`
		SnapshotsBack     []string `json:"snapshots_back"`
		SnapshotsSetAside []string `json:"snapshots_set_aside"`
	}
	must(t, json.Unmarshal(e.cli("agent", "rollback", fqdn, "--to", good.UTC().Format(time.RFC3339)), &p))
	want := append([]string(nil), ids[:2]...)
	slices.Sort(want)
	if p.Refused != "" || !slices.Equal(p.SnapshotsBack, want) || len(p.SnapshotsSetAside) != 0 {
		t.Fatalf("preview: %+v", p)
	}

	// Rolled back: the agent paused and resumed, restic check passed, and the alert is gone.
	before := time.Now()
	time.Sleep(time.Second)
	r := e.rollback(fqdn, good)
	if r.Status != "success" || r.CheckPassed == nil || !*r.CheckPassed || !slices.Equal(r.SnapshotsBack, want) {
		t.Fatalf("rollback: %+v", r)
	}
	if d, _ := e.hostRaw(fqdn)["disappeared"].(float64); d != 0 || len(e.snapshotIDs(fqdn)) != 3 {
		t.Fatalf("after the rollback: disappeared %v, snapshots %v", d, e.snapshotIDs(fqdn))
	}
	logb, _ := os.ReadFile(e.logFile(filepath.Base(a.dir)))
	if !strings.Contains(string(logb), "paused by the server") || !strings.Contains(string(logb), "resumed") {
		t.Fatal("the agent was not paused and resumed")
	}

	// The rollback rolled back: the two are set aside again, by Holdfast this time, so no alert.
	time.Sleep(time.Second)
	r2 := e.rollback(fqdn, before)
	if r2.Status != "success" || !slices.Equal(r2.SnapshotsSetAside, want) || r2.CheckPassed == nil || !*r2.CheckPassed {
		t.Fatalf("rolling the rollback back: %+v", r2)
	}
	if d, _ := e.hostRaw(fqdn)["disappeared"].(float64); d != 0 || len(e.snapshotIDs(fqdn)) != 1 {
		t.Fatalf("after rolling back the rollback: disappeared %v, snapshots %v", d, e.snapshotIDs(fqdn))
	}

	// The agent backs up again.
	n := countStatus(e.events(fqdn), "data", "success")
	e.cli("job", "run", fqdn, "data")
	waitStatus(t, e, fqdn, "data", "success", n+1)
}
