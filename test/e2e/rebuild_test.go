package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

// M9 (plan-m9 §3, decisions 040 and 042): there is one server and no standby, so a lost one is replaced by a new one
// on the same bucket, from the nightly export.
//   - the export: written to meta/ in the bucket and the off-site bucket, encrypted to the escrow's age key;
//   - the database lost, and loaded again with holdfast-server restore-export -latest into an empty one, which it
//     refuses to do twice, or with another key;
//   - the agent enrolled before the export reconnects with its certificate, without a new code; it runs a change made
//     after the export, which the server holds back and shows until the host is changed again (Q6);
//   - plan-m10: restore-export takes the recovery file, and writes the credentials key from it; the export carries the
//     server's files; a host added after the export comes back by itself from its escrow file, with its snapshots,
//     and enrolls again with a new code.
func TestRebuildFromExport(t *testing.T) {
	src := smallSource(t)
	e := newEnvWith(t, envOpts{Offsite: true})
	const web, db = "web01.example.com", "db01.example.com"
	e.startAgent(web, e.addHost(web, "data", "--path", src, "--daily", "02:00"))
	e.waitProgress(web, "first_backup_done", 2*time.Minute)
	before := e.snapshotIDs(web)

	// The export, written to both buckets with their maintenance keys.
	var ex struct {
		Name         string `json:"name"`
		Bytes        int64  `json:"bytes"`
		Error        string `json:"error"`
		Offsite      bool   `json:"offsite"`
		OffsiteError string `json:"offsite_error"`
	}
	must(t, json.Unmarshal(e.cli("export", "now"), &ex))
	if ex.Error != "" || ex.Bytes == 0 || !ex.Offsite || ex.OffsiteError != "" || ex.Name != time.Now().UTC().Format("2006-01-02")+".age" {
		t.Fatalf("export now: %+v", ex)
	}
	for _, b := range []string{e.bucket, e.offBucket} {
		if _, err := s3(t).StatObject(context.Background(), b, "meta/"+ex.Name, minio.StatObjectOptions{}); err != nil {
			t.Fatalf("meta/%s in %s: %v", ex.Name, b, err)
		}
	}
	var list struct {
		On     bool              `json:"on"`
		Recent []json.RawMessage `json:"recent"`
	}
	must(t, json.Unmarshal(e.cli("export", "list"), &list))
	if !list.On || len(list.Recent) != 1 {
		t.Fatalf("export list: %+v", list)
	}

	// After the export: a job the agent runs, and a host with a backup. Both are lost with the database.
	e.cli("job", "set", web, "etc", "--path", src, "--daily", "03:00", "-m", "after the export", testAccept)
	waitFor(t, "web01 runs the new job", time.Minute, func() bool { return e.host(web).InSync })
	ahead := e.host(web).Agent.AppliedRevision
	dbAgent := e.startAgent(db, e.addHost(db, "data", "--path", src, "--daily", "02:00"))
	e.waitProgress(db, "first_backup_done", 2*time.Minute)
	waitFor(t, "db01's escrow file", time.Minute, func() bool { return e.host(db).Escrow.Current })
	dbSnaps := e.snapshotIDs(db)

	// The recovery file of this server, which was set up with its own age key: the credentials key, and the age key
	// added below it, as the admin does (plan-m10 item 3).
	key := filepath.Join(e.dir, "holdfast-recovery.txt")
	cmd := exec.Command(filepath.Join(binDir, "holdfast-server"), "recovery-file", "-config", e.cfgPath, key)
	cmd.Env = e.srvEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recovery-file: %v %s", err, out)
	}
	rf, err := os.OpenFile(key, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = rf.WriteString(e.escrowKey.String() + "\n")
	must(t, err)
	must(t, rf.Close())

	// The server is lost, with its database and its credentials key. A new, empty database is made.
	e.srv.kill(syscall.SIGTERM)
	e.loseDatabase()
	credKey := filepath.Join(filepath.Dir(e.cfgPath), "credentials.key")
	must(t, os.Remove(credKey))

	other, _ := age.GenerateX25519Identity()
	otherKey := filepath.Join(e.dir, "other-age.key")
	must(t, os.WriteFile(otherKey, []byte(other.String()+"\n"), 0o600))
	if out, err := e.restoreExport(otherKey); err == nil || !strings.Contains(out, "age key") {
		t.Fatalf("restore-export with another age key: %v\n%s", err, out)
	}
	out, err := e.restoreExport(key)
	if err != nil || !strings.Contains(out, "restored: head revision") || !strings.Contains(out, "loaded the offsite tables") ||
		!strings.Contains(out, "wrote the credentials key from the recovery file") || !strings.Contains(out, "server.yaml.from-export") ||
		!strings.Contains(out, db+" came back from escrow/") {
		t.Fatalf("restore-export: %v\n%s", err, out)
	}
	if st, err := os.Stat(credKey); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the credentials key written from the recovery file: %v %v", st, err)
	}
	if out, err := e.restoreExport(key); err == nil || !strings.Contains(out, "new, empty database") {
		t.Fatalf("restore-export into a database that is not empty: %v\n%s", err, out)
	}

	// The new server: web01's agent reconnects by itself, its snapshots are listed, and the restore is audited.
	e.startServer()
	waitFor(t, "web01's agent back", time.Minute, func() bool { return e.host(web).Online })
	if ids := e.snapshotIDs(web); !containsAll(ids, before) {
		t.Fatalf("web01's snapshots after the restore: %v, want %v", ids, before)
	}
	restored := false
	for _, a := range e.audit("") {
		restored = restored || a.Action == "server.restored" && a.Detail["export"] == ex.Name
	}
	if !restored {
		t.Fatal("the restore is not in the audit log")
	}
	var after struct {
		Restored *struct {
			ExportName string `json:"export_name"`
		} `json:"restored"`
	}
	must(t, json.Unmarshal(e.cli("export", "list"), &after))
	if after.Restored == nil || after.Restored.ExportName != ex.Name {
		t.Fatalf("export list after the restore: %+v", after)
	}
	if hosts := string(e.offsite("host", "list")); !strings.Contains(hosts, web) {
		t.Fatalf("the off-site tables after the restore: %s", hosts)
	}

	// web01 runs the job made after the export: the server holds its config back and says so (Q6).
	waitFor(t, "web01 shown ahead of the server", time.Minute, func() bool { return hasProblem(e.host(web), "ahead") })
	if h := e.host(web); h.Agent.AppliedRevision != ahead || h.State != "enrolled" {
		t.Fatalf("web01 ahead: %+v", h)
	}
	// A change to the host is sent as usual, and it backs up through the gateway again.
	e.cli("job", "set", web, "data", "--path", src, "--daily", "04:00", "-m", "after the restore", testAccept)
	waitFor(t, "web01 in sync again", time.Minute, func() bool { h := e.host(web); return h.InSync && !hasProblem(h, "ahead") })
	n := countStatus(e.events(web), "data", "success")
	e.cli("job", "run", web, "data")
	waitStatus(t, e, web, "data", "success", n+1)

	// db01 was added after the export: restore-export brought it back from its escrow file, with its repository and
	// password and no job, and it enrolls again with a new code (plan-m10 item 5).
	if h := e.host(db); h.State != "awaiting_enrollment" || len(h.Jobs) != 0 {
		t.Fatalf("db01 after the restore: %+v", h)
	}
	// host import still checks what it is given: db01's file is not web01's.
	f := e.escrowFile(db)
	plain, _ := json.Marshal(f)
	file := filepath.Join(e.dir, "db01-escrow.json")
	must(t, os.WriteFile(file, plain, 0o600))
	if out, err := e.cliErr("host", "import", web, "--escrow-file", file); err == nil || !strings.Contains(string(out), "db01") {
		t.Fatalf("importing db01's file as web01: %v %s", err, out)
	}
	var code struct {
		Code string `json:"code"`
	}
	must(t, json.Unmarshal(e.cli("host", "enroll-code", db), &code))
	dbAgent.kill(syscall.SIGTERM)
	e.startAgent(db, code.Code)
	waitFor(t, "db01 back", time.Minute, func() bool { return e.host(db).Online })
	if ids := e.snapshotIDs(db); !containsAll(ids, dbSnaps) {
		t.Fatalf("db01's snapshots after the import: %v, want %v", ids, dbSnaps)
	}
	if h := e.host(db); hasProblem(h, "foreign_keys") || len(h.Jobs) != 0 {
		t.Fatalf("db01 after the import: %+v", h)
	}
	e.cli("job", "set", db, "data", "--path", src, "--daily", "02:00", "-m", "set again after the import", testAccept)
	e.cli("job", "run", db, "data")
	waitStatus(t, e, db, "data", "success", 1)
	if ids := e.snapshotIDs(db); len(ids) != len(dbSnaps)+1 {
		t.Fatalf("db01's snapshots after a backup: %v", ids)
	}
}

// loseDatabase drops the server's database and makes it again, empty, as a rebuilt server's is.
func (e *env) loseDatabase() {
	e.t.Helper()
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(e.dsn)
	must(e.t, err)
	admin, err := pgx.Connect(ctx, os.Getenv("HOLDFAST_TEST_PG"))
	must(e.t, err)
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, "DROP DATABASE "+cfg.Database+" WITH (FORCE)")
	must(e.t, err)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+cfg.Database)
	must(e.t, err)
}

// restoreExport runs holdfast-server restore-export -latest with the age key file, as root on a rebuilt server would.
func (e *env) restoreExport(key string) (string, error) {
	cmd := exec.Command(filepath.Join(binDir, "holdfast-server"), "restore-export", "-config", e.cfgPath, "-identity", key, "-latest")
	cmd.Env = e.srvEnv
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}
