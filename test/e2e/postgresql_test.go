package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/agent"
)

// PostgreSQL jobs (decision 055) run against a real PostgreSQL 18 server, started here as its own user, with peer
// authentication on its socket and scram-sha-256 over TLS, which the agent verifies (sslmode=verify-full). The e2e
// image installs it, and the clients of version 18 and 16 (test/e2e/Dockerfile).
const (
	pgBin18 = "/usr/lib/postgresql/18/bin"
	pgBin16 = "/usr/lib/postgresql/16/bin"
)

type pgServer struct {
	dir, data, sock, ca string
	port                int
	uid, gid            uint32
}

// startPostgres runs a PostgreSQL 18 server for one test, as the user postgres, on loopback and a socket of its own.
func startPostgres(t *testing.T) *pgServer { return startPostgresFrom(t, pgBin18) }

// startPostgresFrom runs the PostgreSQL server in bin, such as PostgreSQL 16's.
func startPostgresFrom(t *testing.T, bin string) *pgServer {
	t.Helper()
	requireRoot(t)
	if _, err := os.Stat(filepath.Join(bin, "postgres")); err != nil {
		t.Skip("needs PostgreSQL's server in " + bin + " (test/e2e/Dockerfile installs it)")
	}
	u, err := user.Lookup("postgres")
	must(t, err)
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	dir, err := os.MkdirTemp("", "hf-pg-")
	must(t, err)
	must(t, os.Chmod(dir, 0o755))
	must(t, os.Chown(dir, uid, gid)) // the server writes its log here
	p := &pgServer{dir: dir, data: filepath.Join(dir, "data"), sock: filepath.Join(dir, "sock"), ca: filepath.Join(dir, "ca.pem"),
		port: freePort(t), uid: uint32(uid), gid: uint32(gid)}
	for _, d := range []string{p.sock, p.data} {
		must(t, os.Mkdir(d, 0o700))
		must(t, os.Chown(d, uid, gid))
	}

	// TLS: a CA, and a certificate for 127.0.0.1 signed by it, as CloudNativePG makes them. The agent gets the CA as
	// sslrootcert.
	caPEM, crtPEM, keyPEM := pgCerts(t)
	must(t, os.WriteFile(p.ca, caPEM, 0o644))
	for _, f := range []struct {
		b    []byte
		name string
	}{{crtPEM, "server.crt"}, {keyPEM, "server.key"}} {
		dst := filepath.Join(dir, f.name)
		must(t, os.WriteFile(dst, f.b, 0o600))
		must(t, os.Chown(dst, uid, gid))
	}

	p.asPostgres(t, filepath.Join(bin, "initdb"), "-D", p.data, "-U", "postgres", "--auth-local=peer", "--auth-host=scram-sha-256",
		"--encoding=UTF8", "--locale=C.UTF-8")
	conf := fmt.Sprintf("listen_addresses = '127.0.0.1'\nport = %d\nunix_socket_directories = '%s'\nssl = on\nssl_cert_file = '%s'\nssl_key_file = '%s'\n",
		p.port, p.sock, filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	f, err := os.OpenFile(filepath.Join(p.data, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString(conf)
	must(t, err)
	f.Close()
	// Peer authentication maps root, which the agent and this test run as, to the users they sign in as.
	must(t, os.WriteFile(filepath.Join(p.data, "pg_hba.conf"), []byte("local all all peer map=hf\nhostssl all all 127.0.0.1/32 scram-sha-256\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(p.data, "pg_ident.conf"), []byte("hf root postgres\nhf root holdfast_backup\nhf root holdfast_restore\nhf postgres postgres\n"), 0o600))
	for _, n := range []string{"pg_hba.conf", "pg_ident.conf"} {
		must(t, os.Chown(filepath.Join(p.data, n), uid, gid))
	}
	p.asPostgres(t, filepath.Join(bin, "pg_ctl"), "-D", p.data, "-l", filepath.Join(dir, "server.log"), "-w", "start")
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "server.log"))
			t.Logf("==== postgresql ====\n%s", tail(string(b), 6000))
		}
		cmd := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", p.data, "-m", "immediate", "stop")
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: p.uid, Gid: p.gid}}
		_ = cmd.Run()
		_ = os.RemoveAll(dir)
	})
	return p
}

// pgCerts makes a CA and a server certificate for 127.0.0.1 signed by it, with names and key identifiers of their
// own, which OpenSSL needs to tell them apart.
func pgCerts(t *testing.T) (caPEM, crtPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "holdfast-e2e-postgresql-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, SubjectKeyId: []byte{1, 2, 3, 4}}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	must(t, err)
	ca, err = x509.ParseCertificate(caDER)
	must(t, err)
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, SubjectKeyId: []byte{5, 6, 7, 8}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &k.PublicKey, caKey)
	must(t, err)
	kb, err := x509.MarshalECPrivateKey(k)
	must(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func (p *pgServer) asPostgres(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: p.uid, Gid: p.gid}}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + p.dir, "LANG=C.UTF-8"}
	cmd.Dir = p.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", filepath.Base(name), err, out)
	}
}

// sql runs statements in a database as the superuser, one at a time, and returns the rows of the last, tab-separated.
func (p *pgServer) sql(t *testing.T, db, stmts string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(pgBin18, "psql"), "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1",
		"-h", p.sock, "-p", strconv.Itoa(p.port), "-U", "postgres", "-d", db)
	cmd.Stdin = strings.NewReader(stmts)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("psql %s: %v\n%s", db, err, out)
	}
	return strings.TrimSpace(string(out))
}

// restrictRE finds the lines pg_dump 18 writes with a random key in a plain dump.
var restrictRE = regexp.MustCompile(`(?m)^\\(un)?restrict .*$`)

// dump is a database in pg_dump's plain format, owners and grants included, to compare two databases exactly. The
// planner's statistics are left out: they are the server's guesses, not data.
func (p *pgServer) dump(t *testing.T, db string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(pgBin18, "pg_dump"), "--no-statistics", "-h", p.sock, "-p", strconv.Itoa(p.port), "-U", "postgres", "-d", db)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pg_dump %s: %v\n%s", db, err, stderr.String())
	}
	return restrictRE.ReplaceAllString(string(out), "")
}

// setPostgreSQL edits the agent's own config, as root on the host or the pod's ConfigMap would: its PostgreSQL client,
// whether it allows restores in place, and where restored files go.
func setPostgreSQL(t *testing.T, a *agentProc, binDir string, inPlace bool, restoreRoot string) {
	t.Helper()
	cfg, err := agent.LoadConfig(a.cfgPath)
	must(t, err)
	cfg.PostgreSQL.BinDir, cfg.PostgreSQL.AllowInPlaceRestore, cfg.Restore.Root = binDir, inPlace, restoreRoot
	must(t, agent.WriteConfig(a.cfgPath, cfg))
}

// addAgent adds an agent with no job, and returns its enrollment code.
func (e *env) addAgent(name string) string {
	e.t.Helper()
	var res struct {
		Enrollment struct{ Code string } `json:"enrollment"`
	}
	out := e.cli("agent", "add", name)
	if err := json.Unmarshal(out, &res); err != nil || res.Enrollment.Code == "" {
		e.t.Fatalf("agent add: %v %s", err, out)
	}
	return res.Enrollment.Code
}

// api calls the HTTP API on the server's socket, as the CLI does, and returns the status and body.
func (e *env) api(method, path string, body any) (int, []byte) {
	e.t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.sock)
	}}}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://holdfast"+path, r)
	must(e.t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	must(e.t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type pgConnView struct {
	Name               string `json:"name"`
	PasswordSet        bool   `json:"password_set"`
	RestorePasswordSet bool   `json:"restore_password_set"`
	LastTest           *struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Result *struct {
			ServerVersion string `json:"server_version"`
			Client        string `json:"client"`
			TLS           string `json:"tls"`
			Databases     []struct {
				Name  string `json:"name"`
				Bytes int64  `json:"bytes"`
				Owner string `json:"owner"`
			} `json:"databases"`
			RestoreUser *struct {
				CreateDB bool     `json:"createdb"`
				ActsAs   []string `json:"acts_as"`
				Error    string   `json:"error"`
			} `json:"restore_user"`
		} `json:"result"`
	} `json:"last_test"`
}

// pgTest tests a connection through the CLI, as people do, and returns what the agent found.
func (e *env) pgTest(agentName, conn string) pgConnView {
	e.t.Helper()
	var v pgConnView
	out := e.cli("agent", "connection", "test", agentName, conn)
	must(e.t, json.Unmarshal(out, &v))
	if v.LastTest == nil || !v.LastTest.OK || v.LastTest.Result == nil {
		e.t.Fatalf("connection test %s %s: %s", agentName, conn, out)
	}
	return v
}

// runJob backs a job up now and waits for its run's result.
func (e *env) runJob(agentName, job string) event {
	e.t.Helper()
	before := time.Now().Add(-time.Second)
	e.cli("job", "run", agentName, job)
	var got event
	waitFor(e.t, agentName+" "+job+" backed up", 3*time.Minute, func() bool {
		for _, ev := range e.events(agentName) {
			if ev.Kind == "backup" && ev.JobID == job && ev.Started.After(before) && ev.Status != "running" {
				got = ev
				return true
			}
		}
		return false
	})
	return got
}

func (e *env) snapshotsOf(agentName, job string) []catSnapshot {
	e.t.Helper()
	var all, out []catSnapshot
	must(e.t, json.Unmarshal(e.cli("snapshot", "list", agentName), &all))
	for _, s := range all {
		if s.JobID == job && s.GoneAt == nil {
			out = append(out, s)
		}
	}
	return out
}

// TestPostgreSQL (decision 055): an agent backs up PostgreSQL databases with pg_dump, through restic's
// --stdin-from-command, from connections set centrally, with passwords that never show; and restores a dump into a new
// database beside the original, in place after a safety dump, or as the file. Restored databases match the original
// exactly. A failing pg_dump makes no snapshot; an old pg_dump is refused (P001); deleting a snapshot needs a reason.
func TestPostgreSQL(t *testing.T) {
	pg := startPostgres(t)
	backupPW, restorePW := "bk-"+randHex(12)+":x\\y", "rs-"+randHex(12)
	pg.sql(t, "postgres", `CREATE ROLE app NOLOGIN;
CREATE ROLE holdfast_backup LOGIN PASSWORD '`+backupPW+`' IN ROLE pg_read_all_data;
CREATE ROLE holdfast_restore LOGIN CREATEDB PASSWORD '`+restorePW+`' IN ROLE app;
CREATE DATABASE app OWNER app;
CREATE DATABASE other OWNER postgres;`)
	pg.sql(t, "app", `SET ROLE app;
CREATE SCHEMA shop;
CREATE TABLE shop.customer (id bigserial PRIMARY KEY, name text NOT NULL, email text UNIQUE, data jsonb, photo bytea, created timestamptz DEFAULT '2026-01-02 03:04:05+00');
INSERT INTO shop.customer (name, email, data, photo) SELECT 'name ' || g, 'c' || g || '@example.test', jsonb_build_object('n', g, 'tags', jsonb_build_array('a', 'b')), decode(md5(g::text), 'hex') FROM generate_series(1, 5000) g;
CREATE TABLE orders (id int PRIMARY KEY, customer bigint REFERENCES shop.customer (id), amount numeric(12, 2), note text);
INSERT INTO orders SELECT g, (g % 5000) + 1, g * 1.5, repeat('x', g % 100) FROM generate_series(1, 20000) g;
CREATE INDEX orders_customer ON orders (customer);
CREATE VIEW shop.big_orders AS SELECT * FROM orders WHERE amount > 1000;
CREATE FUNCTION shop.total(c bigint) RETURNS numeric LANGUAGE sql AS $$ SELECT sum(amount) FROM orders WHERE customer = c $$;
COMMENT ON TABLE orders IS 'Ünïcødé ✓';
SELECT setval('shop.customer_id_seq', 7000);`)
	pg.sql(t, "other", "CREATE TABLE t (id int PRIMARY KEY, v text); INSERT INTO t SELECT g, md5(g::text) FROM generate_series(1, 100) g;")
	original := pg.dump(t, "app")

	e := newEnv(t)
	const name = "pg01" // an agent's name need not be a host's: decision 056
	a := e.enrollAgent(name, e.addAgent(name))
	restoreRoot := filepath.Join(t.TempDir(), "restore")
	setPostgreSQL(t, a, pgBin18, true, restoreRoot)
	e.runAgent(a)
	waitFor(t, "the agent reports its PostgreSQL client", time.Minute, func() bool {
		m := e.hostRaw(name)
		ag, _ := m["agent"].(map[string]any)
		p, _ := ag["postgresql"].(map[string]any)
		return p != nil && strings.HasPrefix(fmt.Sprint(p["client"]), "18.")
	})

	// Two connections: over TCP with TLS verified and passwords, and on the socket with peer authentication.
	pwFile, rpwFile := filepath.Join(t.TempDir(), "pw"), filepath.Join(t.TempDir(), "rpw")
	must(t, os.WriteFile(pwFile, []byte(backupPW+"\n"), 0o600))
	must(t, os.WriteFile(rpwFile, []byte(restorePW), 0o600))
	tlsURL := fmt.Sprintf("postgresql://127.0.0.1:%d/?sslmode=verify-full&sslrootcert=%s", pg.port, pg.ca)
	e.cli("agent", "connection", "set", name, "main", "--url", tlsURL, "--user", "holdfast_backup", "--password-file", pwFile,
		"--restore-user", "holdfast_restore", "--restore-password-file", rpwFile, "-m", "e2e: main over TLS")
	e.cli("agent", "connection", "set", name, "local", "--url", fmt.Sprintf("postgresql:///?host=%s&port=%d", pg.sock, pg.port),
		"--user", "holdfast_backup", "--no-password", "--restore-user", "holdfast_restore")
	// A URL with a password in it is refused: passwords go only into the sealed field.
	if out, err := e.cliErr("agent", "connection", "set", name, "bad", "--url", fmt.Sprintf("postgresql://holdfast_backup:%s@127.0.0.1:%d/", "pw", pg.port),
		"--user", "holdfast_backup", "--no-password"); err == nil {
		t.Fatalf("a URL with a password must be refused: %s", out)
	}

	main := e.pgTest(name, "main")
	r := main.LastTest.Result
	if !strings.HasPrefix(r.ServerVersion, "18.") || !strings.HasPrefix(r.Client, "18.") || !strings.HasPrefix(r.TLS, "TLS") {
		t.Fatalf("the test over TLS: %+v", r)
	}
	var dbs []string
	for _, d := range r.Databases {
		dbs = append(dbs, d.Name+"/"+d.Owner)
	}
	if !slices.Contains(dbs, "app/app") || !slices.Contains(dbs, "other/postgres") {
		t.Fatalf("databases: %v", dbs)
	}
	if ru := r.RestoreUser; ru == nil || !ru.CreateDB || !slices.Contains(ru.ActsAs, "app") || ru.Error != "" {
		t.Fatalf("the restore user's rights: %+v", ru)
	}
	if !main.PasswordSet || !main.RestorePasswordSet {
		t.Fatalf("the passwords must show as set: %+v", main)
	}
	if local := e.pgTest(name, "local"); local.LastTest.Result.TLS != "" || local.PasswordSet {
		t.Fatalf("the socket connection: %+v", local.LastTest.Result)
	}

	// A job each, in one revision: two databases and the roles, every 6 hours. A database the test did not find is a
	// warning (P003), which this test accepts for the job that must fail.
	e.cli("job", "set", name, "--connection", "main", "--database", "app", "--database", "other", "--globals", "--every", "6h", "--from", "00:30",
		testAccept, "-m", "e2e: PostgreSQL jobs")
	if out, err := e.cliErr("job", "set", name, "nosuch", "--connection", "local", "--database", "nosuch", testAccept); err == nil || !strings.Contains(string(out), "P003") {
		t.Fatalf("a database the test did not find must warn with P003: %v %s", err, out)
	}
	e.cli("job", "set", name, "nosuch", "--connection", "local", "--database", "nosuch", testAccept+",P003")
	if out, err := e.cliErr("job", "set", name, "app2", "--connection", "main", "--database", "app", testAccept); err == nil || !strings.Contains(string(out), "same already") {
		t.Fatalf("two jobs of one database must be refused: %v %s", err, out)
	}

	for _, j := range []string{"app", "other", "roles"} {
		if ev := e.runJob(name, j); ev.Status != "success" || ev.SnapshotID == "" {
			t.Fatalf("backup of %s: %+v", j, ev)
		}
	}
	// pg_dump fails: the run fails, with PostgreSQL's reason, and there is no snapshot.
	if ev := e.runJob(name, "nosuch"); ev.Status != "failed" || ev.SnapshotID != "" || !strings.Contains(ev.Error, "nosuch") {
		t.Fatalf("a failing pg_dump: %+v", ev)
	}
	// A new job backs up at once, and the run above once more: the newest snapshot is the run's.
	waitFor(t, "the snapshots in the catalogue", time.Minute, func() bool {
		return len(e.snapshotsOf(name, "app")) >= 1 && len(e.snapshotsOf(name, "roles")) >= 1 && len(e.snapshotsOf(name, "other")) >= 1
	})
	if s := e.snapshotsOf(name, "nosuch"); len(s) != 0 {
		t.Fatalf("a failing pg_dump must leave no snapshot: %+v", s)
	}
	snap := e.snapshotsOf(name, "app")[0].ShortID

	// Passwords show nowhere: not in the API's views, the revision, the audit log or the logs.
	var revs []struct {
		Number int64 `json:"number"`
	}
	must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
	seen := [][]byte{e.cli("agent", "show", name), e.cli("agent", "connection", "list", name), e.cli("audit", "--host", name), e.cli("agent", "events", name)}
	for _, rv := range revs {
		seen = append(seen, e.cli("revision", "show", strconv.FormatInt(rv.Number, 10)))
	}
	for _, l := range e.logs {
		b, _ := os.ReadFile(l)
		seen = append(seen, b)
	}
	for _, b := range seen {
		if bytes.Contains(b, []byte(backupPW)) || bytes.Contains(b, []byte(restorePW)) {
			t.Fatalf("a PostgreSQL password shows:\n%s", tail(string(b), 2000))
		}
	}

	restore := func(args ...string) restoreResult {
		t.Helper()
		var ev restoreResult
		out := e.cli(append([]string{"restore", name}, append(args, "--wait")...)...)
		must(t, json.Unmarshal(out, &ev))
		return ev
	}

	// Into a new database beside the original, named after the dump's time: a dry run first reads the dump and
	// makes nothing.
	if ev := restore(snap, "--to-database", "--dry-run"); ev.Status != "success" || ev.Detail["dry_run"] != "true" {
		t.Fatalf("dry run: %+v", ev)
	}
	ev := restore(snap, "--to-database", "-m", "e2e: check the dump")
	newDB := ev.Detail["database"]
	if ev.Status != "success" || !regexp.MustCompile(`^app-restore-\d{8}-\d{4}$`).MatchString(newDB) || ev.Detail["owner"] != "app" || ev.Detail["tables"] != "2" {
		t.Fatalf("restore into a new database: %+v", ev)
	}
	if got := pg.dump(t, newDB); got != original {
		t.Fatalf("the restored database differs from the original:\n%s", firstDiff(original, got))
	}
	// The snapshot list names that database beforehand, so every client shows the name the restore uses.
	type listedSnap struct {
		ShortID         string `json:"short_id"`
		RestoreDatabase string `json:"restore_database"`
	}
	var listed []listedSnap
	must(t, json.Unmarshal(e.cli("snapshot", "list", name), &listed))
	if i := slices.IndexFunc(listed, func(s listedSnap) bool { return s.ShortID == snap }); i < 0 || listed[i].RestoreDatabase != newDB {
		t.Fatalf("the snapshot list must name the restore's database %s: %+v", newDB, listed)
	}
	if owner := pg.sql(t, "postgres", "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '"+newDB+"'"); owner != "app" {
		t.Fatalf("the new database belongs to %q, not to the original's owner", owner)
	}
	// The name is taken now: the same restore again is refused, and nothing is overwritten.
	if out, err := e.cliErr("restore", name, snap, "--to-database", "--wait"); err == nil || !strings.Contains(string(out), "exists already") {
		t.Fatalf("a second restore into the same name must be refused: %v %s", err, out)
	}

	// In place: only with the database's name typed; a safety dump first; one transaction.
	if out, err := e.cliErr("restore", name, snap, "--in-place"); err == nil || !strings.Contains(string(out), "confirm") {
		t.Fatalf("in place without --confirm: %v %s", err, out)
	}
	pg.sql(t, "app", "DELETE FROM orders WHERE id > 100; UPDATE shop.customer SET name = 'changed'; DROP VIEW shop.big_orders;")
	ev = restore(snap, "--in-place", "--confirm", "app", "-m", "e2e: undo a bad change")
	if ev.Status != "success" || ev.Detail["in_place"] != "true" || ev.Detail["pre_restore"] == "" {
		t.Fatalf("restore in place: %+v", ev)
	}
	if got := pg.dump(t, "app"); got != original {
		t.Fatalf("the database restored in place differs from the original:\n%s", firstDiff(original, got))
	}
	// The safety dump holds the database as it was: it restores, beside it, with the 100 orders left then. Its name
	// is the dump's time to the minute, which the first restore's may share in a fast test: that one goes first.
	pg.sql(t, "postgres", `DROP DATABASE "`+newDB+`"`)
	preID := ev.Detail["pre_restore"][:8]
	waitFor(t, "the safety dump in the catalogue", time.Minute, func() bool {
		return slices.ContainsFunc(e.snapshotsOf(name, "app"), func(s catSnapshot) bool { return s.ShortID == preID })
	})
	pre := restore(preID, "--to-database")
	if pre.Status != "success" {
		t.Fatalf("restoring the safety dump: %+v", pre)
	}
	if n := pg.sql(t, pre.Detail["database"], "SELECT count(*) FROM orders"); n != "100" {
		t.Fatalf("the safety dump holds %s orders, not the 100 left before the restore", n)
	}
	var inPlaceAudited bool
	for _, en := range e.audit(name) {
		inPlaceAudited = inPlaceAudited || (en.Action == "restore.requested" && en.Detail["in_place"] == true && en.Detail["database"] == "app" &&
			en.Detail["message"] == "e2e: undo a bad change")
	}
	if !inPlaceAudited {
		t.Fatalf("the restore in place, with its reason, must be in the audit log: %+v", e.audit(name))
	}

	// A database dropped with its owner role: its dump still restores beside it, owned by the restore user, without the
	// grants to roles that are gone.
	pg.sql(t, "postgres", "CREATE ROLE goner NOLOGIN; CREATE ROLE reader NOLOGIN; CREATE DATABASE gone OWNER goner;")
	pg.sql(t, "gone", "SET ROLE goner; CREATE TABLE notes (id int PRIMARY KEY, body text); INSERT INTO notes SELECT g, 'note ' || g FROM generate_series(1, 42) g; GRANT SELECT ON notes TO reader;")
	e.pgTest(name, "main") // the test finds the new database, so its job needs no P003
	e.cli("job", "set", name, "gone", "--connection", "main", "--database", "gone", testAccept)
	if ev := e.runJob(name, "gone"); ev.Status != "success" {
		t.Fatalf("backup of gone: %+v", ev)
	}
	var goneSnap string
	waitFor(t, "gone's snapshot in the catalogue", time.Minute, func() bool {
		s := e.snapshotsOf(name, "gone")
		if len(s) > 0 {
			goneSnap = s[0].ShortID
		}
		return goneSnap != ""
	})
	pg.sql(t, "postgres", "DROP DATABASE gone; DROP ROLE goner; DROP ROLE reader;")
	// Its job removed too, as when a database is retired: the snapshot still knows it is gone's dump, from the config
	// history, and names the restore's database.
	e.cli("job", "remove", name, "gone", "-m", "e2e: gone was dropped")
	type pgListed struct {
		ShortID    string `json:"short_id"`
		PostgreSQL *struct {
			Database string `json:"database"`
		} `json:"postgresql"`
		RestoreDatabase string `json:"restore_database"`
	}
	var goneListed []pgListed
	must(t, json.Unmarshal(e.cli("snapshot", "list", name), &goneListed))
	if i := slices.IndexFunc(goneListed, func(s pgListed) bool { return s.ShortID == goneSnap }); i < 0 || goneListed[i].PostgreSQL == nil ||
		goneListed[i].PostgreSQL.Database != "gone" || !strings.HasPrefix(goneListed[i].RestoreDatabase, "gone-restore-") {
		t.Fatalf("a removed job's dump must still be listed as gone's: %+v", goneListed)
	}
	ev = restore(goneSnap, "--to-database", "-m", "e2e: a database, its owner and its job are gone")
	if ev.Status != "success" || ev.Detail["no_owner"] != "true" || ev.Detail["owner"] != "holdfast_restore" {
		t.Fatalf("restoring a database whose owner is gone: %+v", ev)
	}
	if n := pg.sql(t, ev.Detail["database"], "SELECT count(*) || ' ' || pg_get_userbyid(c.relowner) FROM notes, pg_class c WHERE c.relname = 'notes' GROUP BY c.relowner"); n != "42 holdfast_restore" {
		t.Fatalf("the copy of gone: %q, not its 42 notes owned by the restore user", n)
	}

	// As the file: the dump, which pg_restore reads; the roles' file has no password.
	ev = restore(snap)
	dumpFile := filepath.Join(ev.Detail["directory"], "app.dump")
	if ev.Status != "success" {
		t.Fatalf("restore as the file: %+v", ev)
	}
	if out, err := exec.Command(filepath.Join(pgBin18, "pg_restore"), "--list", dumpFile).CombinedOutput(); err != nil || !strings.Contains(string(out), "TABLE DATA public orders") {
		t.Fatalf("pg_restore --list of the restored file: %v\n%s", err, tail(string(out), 2000))
	}
	roles := e.snapshotsOf(name, "roles")[0].ShortID
	if out, err := e.cliErr("restore", name, roles, "--to-database"); err == nil || !strings.Contains(string(out), "roles and globals") {
		t.Fatalf("the roles restore only as the file: %v %s", err, out)
	}
	ev = restore(roles)
	g, err := os.ReadFile(filepath.Join(ev.Detail["directory"], "globals.sql"))
	if err != nil || !strings.Contains(string(g), "CREATE ROLE holdfast_restore") || strings.Contains(string(g), "SCRAM-SHA-256") || strings.Contains(string(g), restorePW) {
		t.Fatalf("the roles' file: %v\n%s", err, tail(string(g), 2000))
	}

	// Deleting a snapshot needs a reason, in the API as in the CLI, and the reason is audited.
	other := e.snapshotsOf(name, "other")[0].ShortID
	if out, err := e.cliErr("snapshot", "delete", name, other); err == nil || !strings.Contains(string(out), "reason") {
		t.Fatalf("a deletion without a reason, in the CLI: %v %s", err, out)
	}
	if code, body := e.api("POST", "/api/v1/agents/"+name+"/snapshots/"+other+"/delete", map[string]any{"message": " "}); code != http.StatusBadRequest && code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "reason") {
		t.Fatalf("a deletion without a reason, in the API: %d %s", code, body)
	}
	e.cli("snapshot", "delete", name, other, "--reason", "e2e: a test of the reason")
	var reasonAudited bool
	for _, en := range e.audit(name) {
		reasonAudited = reasonAudited || (strings.HasPrefix(en.Action, "snapshot.delete") && en.Detail["message"] == "e2e: a test of the reason")
	}
	if !reasonAudited {
		t.Fatalf("the deletion's reason must be in the audit log: %+v", e.audit(name))
	}

	// An agent whose pg_dump is older than the server: its test says so, and a job on it is refused (P001, an error).
	const old = "pg16"
	b := e.enrollAgent(old, e.addAgent(old))
	setPostgreSQL(t, b, pgBin16, false, restoreRoot)
	e.runAgent(b)
	e.cli("agent", "connection", "set", old, "main", "--url", tlsURL, "--user", "holdfast_backup", "--password-file", pwFile)
	if v := e.pgTest(old, "main"); !strings.HasPrefix(v.LastTest.Result.Client, "16.") {
		t.Fatalf("the old client: %+v", v.LastTest.Result)
	}
	if out, err := e.cliErr("job", "set", old, "--connection", "main", "--database", "app", testAccept); err == nil || !strings.Contains(string(out), "P001") {
		t.Fatalf("pg_dump 16 against PostgreSQL 18 must be refused with P001: %v %s", err, out)
	}
	if !slices.ContainsFunc(e.host(old).Problems, func(p problem) bool { return p.Code == "P001" }) {
		t.Fatalf("the agent must show P001: %+v", e.host(old).Problems)
	}
}

// TestPostgreSQLOlderServer (decision 055): the agent's client is newer than the server, as a pod with PostgreSQL 18's
// client that backs up a PostgreSQL 16 cluster. Its dumps restore there, into a new database and in place, the same as
// the original: pg_restore's SQL leaves out the settings PostgreSQL 16 does not know (transaction_timeout, new in 17),
// which pg_restore straight into the database sends, failing the load (found in the lab).
func TestPostgreSQLOlderServer(t *testing.T) {
	pg := startPostgresFrom(t, pgBin16)
	if v := pg.sql(t, "postgres", "SHOW server_version_num"); !strings.HasPrefix(v, "16") {
		t.Fatalf("not PostgreSQL 16: %s", v)
	}
	pg.sql(t, "postgres", `CREATE ROLE app NOLOGIN;
CREATE ROLE holdfast_backup LOGIN IN ROLE pg_read_all_data;
CREATE ROLE holdfast_restore LOGIN CREATEDB IN ROLE app;
CREATE DATABASE app OWNER app;`)
	pg.sql(t, "app", `SET ROLE app;
CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL, data jsonb);
INSERT INTO items SELECT g, 'item ' || g, jsonb_build_object('n', g) FROM generate_series(1, 2000) g;
CREATE VIEW big AS SELECT * FROM items WHERE id > 1000;`)
	original := pg.dump(t, "app")

	e := newEnv(t)
	const name = "pg-old"
	a := e.enrollAgent(name, e.addAgent(name))
	setPostgreSQL(t, a, pgBin18, true, filepath.Join(t.TempDir(), "restore"))
	e.runAgent(a)
	e.cli("agent", "connection", "set", name, "main", "--url", fmt.Sprintf("postgresql:///?host=%s&port=%d", pg.sock, pg.port),
		"--user", "holdfast_backup", "--no-password", "--restore-user", "holdfast_restore")
	if r := e.pgTest(name, "main").LastTest.Result; !strings.HasPrefix(r.ServerVersion, "16.") || !strings.HasPrefix(r.Client, "18.") {
		t.Fatalf("the test of PostgreSQL 16 with pg_dump 18: %+v", r)
	}
	e.cli("job", "set", name, "app", "--connection", "main", "--database", "app", testAccept)
	if ev := e.runJob(name, "app"); ev.Status != "success" {
		t.Fatalf("backup: %+v", ev)
	}
	var snap string
	waitFor(t, "the snapshot in the catalogue", time.Minute, func() bool {
		if s := e.snapshotsOf(name, "app"); len(s) > 0 {
			snap = s[0].ShortID
		}
		return snap != ""
	})
	restore := func(args ...string) restoreResult {
		t.Helper()
		var ev restoreResult
		must(t, json.Unmarshal(e.cli(append([]string{"restore", name}, append(args, "--wait")...)...), &ev))
		return ev
	}
	ev := restore(snap, "--to-database", "-m", "e2e: into PostgreSQL 16")
	if ev.Status != "success" || ev.Detail["tables"] != "1" {
		t.Fatalf("restore into a new database on PostgreSQL 16: %+v", ev)
	}
	if got := pg.dump(t, ev.Detail["database"]); got != original {
		t.Fatalf("the restored database differs from the original:\n%s", firstDiff(original, got))
	}
	pg.sql(t, "app", "DELETE FROM items WHERE id > 10; DROP VIEW big;")
	if ev := restore(snap, "--in-place", "--confirm", "app", "-m", "e2e: in place on PostgreSQL 16"); ev.Status != "success" {
		t.Fatalf("restore in place on PostgreSQL 16: %+v", ev)
	}
	if got := pg.dump(t, "app"); got != original {
		t.Fatalf("the database restored in place differs from the original:\n%s", firstDiff(original, got))
	}
}

// firstDiff shows where two texts first differ, with some context.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d:\n- %s\n+ %s", i+1, x, y)
		}
	}
	return "(the same)"
}
