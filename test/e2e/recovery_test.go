package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/kabsdk/holdfast/internal/agent"
)

// M6 (plan-m6 §3): dumps, a restore onto another host, and rolling a repository back.

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

func TestDump(t *testing.T) {
	pgDump, err1 := exec.LookPath("pg_dump")
	psql, err2 := exec.LookPath("psql")
	if err1 != nil || err2 != nil {
		t.Skip("needs PostgreSQL's client tools: make e2e's image has them")
	}
	ctx := context.Background()
	admin := os.Getenv("HOLDFAST_TEST_PG")
	cfg, err := pgx.ParseConfig(admin)
	must(t, err)
	pgEnv := map[string]string{"PGHOST": cfg.Host, "PGPORT": fmt.Sprint(cfg.Port), "PGUSER": cfg.User, "PGPASSWORD": cfg.Password}
	db := "hf_dump_" + randHex(4)
	conn, err := pgx.Connect(ctx, admin)
	must(t, err)
	defer conn.Close(ctx)
	for _, d := range []string{db, db + "_check"} {
		_, err := conn.Exec(ctx, "CREATE DATABASE "+d)
		must(t, err)
		t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+d+" WITH (FORCE)") })
	}
	src, err := pgx.Connect(ctx, strings.Replace(admin, "/postgres?", "/"+db+"?", 1))
	must(t, err)
	_, err = src.Exec(ctx, `CREATE TABLE orders (id int PRIMARY KEY, note text); INSERT INTO orders SELECT g, 'order ' || g FROM generate_series(1, 1000) g`)
	must(t, err)
	src.Close(ctx)

	e := newEnv(t)
	const fqdn = "db01.example.com"
	restoreRoot := filepath.Join(e.dir, "restore-db01")
	a := e.enrollAgent(fqdn, e.addHost(fqdn, "etc", "--path", smallSource(t), "--daily", "02:00"))
	editAgent(t, a, func(c *agent.Config) {
		c.Restore.Root = restoreRoot
		c.Dumps = map[string]agent.Dump{
			"pg":     {Command: []string{pgDump, "--no-owner", "--dbname", db}, Env: pgEnv, Filename: "orders.sql"},
			"env":    {Command: []string{"/usr/bin/env"}, Filename: "env.txt"},
			"whoami": {Command: []string{"/usr/bin/id", "-un"}, User: "nobody"},
			"broken": {Command: []string{"/bin/sh", "-c", "echo partial; exit 3"}},
		}
	})
	e.runAgent(a)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)

	// A dump the host does not define is refused (D001); the others are jobs like any other.
	if out, err := e.cliErr("job", "set", fqdn, "nope", "--dump", "nope", "--daily", "03:00", testAccept); err == nil || !strings.Contains(string(out), "D001") {
		t.Fatalf("an unknown dump: %v %s", err, out)
	}
	for _, d := range []string{"pg", "env", "whoami", "broken"} {
		e.cli("job", "set", fqdn, d, "--dump", d, "--daily", "03:00", testAccept)
		e.cli("job", "run", fqdn, d)
	}
	for _, d := range []string{"pg", "env", "whoami"} {
		waitStatus(t, e, fqdn, d, "success", 1)
	}
	waitStatus(t, e, fqdn, "broken", "failed", 1)
	var snaps []struct {
		JobID string `json:"job_id"`
	}
	must(t, json.Unmarshal(e.cli("snapshot", "list", fqdn, "--refresh"), &snaps))
	for _, s := range snaps {
		if s.JobID == "broken" {
			t.Fatal("a dump whose command failed made a snapshot")
		}
	}

	// Restored into a new directory only, then loaded and compared.
	pgSnap := e.latestSnapshot(fqdn, "pg")
	if out, err := e.cliErr("restore", fqdn, pgSnap, "--in-place"); err == nil || !strings.Contains(string(out), "new directory") {
		t.Fatalf("a dump in place: %v %s", err, out)
	}
	ev, err := e.restoreWait(fqdn, pgSnap)
	if err != nil || ev.Status != "success" {
		t.Fatalf("restore the dump: %+v %v", ev, err)
	}
	load := exec.Command(psql, "-v", "ON_ERROR_STOP=1", "-q", "--dbname", db+"_check", "-f", filepath.Join(ev.Detail["directory"], "orders.sql"))
	load.Env = os.Environ()
	for k, v := range pgEnv {
		load.Env = append(load.Env, k+"="+v)
	}
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("loading the dump: %v\n%s", err, out)
	}
	check, err := pgx.Connect(ctx, strings.Replace(admin, "/postgres?", "/"+db+"_check?", 1))
	must(t, err)
	var n int
	must(t, check.QueryRow(ctx, "SELECT count(*) FROM orders WHERE note LIKE 'order %'").Scan(&n))
	check.Close(ctx)
	if n != 1000 {
		t.Fatalf("the loaded dump has %d rows, want 1000", n)
	}

	// The dump command never sees restic's environment: no S3 key, no password file.
	ev, err = e.restoreWait(fqdn, e.latestSnapshot(fqdn, "env"))
	if err != nil || ev.Status != "success" {
		t.Fatalf("restore the env dump: %+v %v", ev, err)
	}
	envOut, err := os.ReadFile(filepath.Join(ev.Detail["directory"], "env.txt"))
	must(t, err)
	if strings.Contains(string(envOut), "AWS_") || strings.Contains(string(envOut), "RESTIC_") || !strings.Contains(string(envOut), "PATH=/usr/sbin") {
		t.Fatalf("the dump command's environment:\n%s", envOut)
	}
	// And it runs as its user.
	ev, err = e.restoreWait(fqdn, e.latestSnapshot(fqdn, "whoami"))
	if err != nil || ev.Status != "success" {
		t.Fatalf("restore the whoami dump: %+v %v", ev, err)
	}
	if b, _ := os.ReadFile(filepath.Join(ev.Detail["directory"], "whoami.dump")); strings.TrimSpace(string(b)) != "nobody" {
		t.Fatalf("the dump ran as %q, not nobody", b)
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

	// The admin's read-only key for A's prefix; and A's own key, which can write.
	p := "repos/" + a
	roID, roSecret := e.makeUser(`{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetBucketLocation"],"Resource":["arn:aws:s3:::` + e.bucket + `"]},
 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::` + e.bucket + `"],"Condition":{"StringLike":{"s3:prefix":["` + p + `","` + p + `/*"]}}},
 {"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + e.bucket + `/` + p + `/*"]}]}`)
	roFile := filepath.Join(e.dir, "readonly.env")
	must(t, os.WriteFile(roFile, []byte("AWS_ACCESS_KEY_ID="+roID+"\nAWS_SECRET_ACCESS_KEY="+roSecret+"\n"), 0o600))
	rwFile, _ := e.makeKey(a, "")

	// The host's opt-in is checked first: it costs no S3 call.
	if out, err := e.cliErr("restore", a, snap, "--to-host", b, "--s3-key-file", roFile); err == nil || !strings.Contains(string(out), "allow_other_hosts") {
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
	if out, err := e.cliErr("restore", a, snap, "--to-host", b, "--s3-key-file", rwFile); err == nil || !strings.Contains(string(out), "B015") {
		t.Fatalf("a key that can write: %v %s", err, out)
	}
	if out, err := e.cliErr("restore", a, snap, "--to-host", b, "--s3-key-file", roFile, "--in-place"); err == nil {
		t.Fatalf("in place onto another host: %s", out)
	}
	ev, err := e.restoreWait(a, snap, "--to-host", b, "--s3-key-file", roFile, "-m", "e2e: A is gone")
	if err != nil || ev.Status != "success" || ev.Detail["source_host"] != a || !strings.HasPrefix(ev.Detail["directory"], restoreRoot+"/") {
		t.Fatalf("restore onto B: %+v %v", ev, err)
	}
	got, err := os.ReadFile(filepath.Join(ev.Detail["directory"], src, "invoice.txt"))
	if err != nil || string(got) != "invoice 42\n" {
		t.Fatalf("the restored file: %q %v", got, err)
	}
	// B kept A's secrets only for that restore.
	left, _ := filepath.Glob(filepath.Join(bAgent.dir, "state", "source-*"))
	if len(left) != 0 {
		t.Fatalf("B kept A's credentials: %v", left)
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
	out, err := e.cliErr("host", "rollback", fqdn, "--to", to.UTC().Format(time.RFC3339), "--confirm", fqdn, "--wait", "-m", "e2e")
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
	must(t, json.Unmarshal(e.cli("host", "rollback", fqdn, "--to", good.UTC().Format(time.RFC3339)), &p))
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
