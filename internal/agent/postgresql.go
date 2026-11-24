package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
)

// PostgreSQL jobs (decision 055). The agent runs PostgreSQL's own programs from postgresql.bin_dir in its agent.yaml:
// pg_dump and pg_dumpall through restic's --stdin-from-command to back up, psql to test a connection and to create a
// database, and pg_restore to load a dump. Each runs with a clean environment: no RESTIC_* or AWS_* variable, the
// password in a pgpass file only the agent reads, nothing secret on a command line.

// pgUser and pgRestoreUser are which of a connection's users a command signs in as.
const (
	pgUser        = "backup"
	pgRestoreUser = "restore"
)

// pgVersionRE finds a version such as 18.1 or 16.10 in pg_dump --version: "pg_dump (PostgreSQL) 18.1 (Debian 18.1-1)".
var pgVersionRE = regexp.MustCompile(`\(PostgreSQL\) ([0-9]+(\.[0-9]+)?)`)

// pgClient is the version of pg_dump in bin_dir, or "" if there is none that runs.
func (a *Agent) pgClient(ctx context.Context) string {
	if a.cfg.PostgreSQL.BinDir == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, filepath.Join(a.cfg.PostgreSQL.BinDir, "pg_dump"), "--version").Output()
	if err != nil {
		return ""
	}
	if m := pgVersionRE.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// pgFacts are what the agent reports of its PostgreSQL client; nil without a bin_dir.
func (a *Agent) pgFacts(ctx context.Context) *proto.PGFacts {
	if a.cfg.PostgreSQL.BinDir == "" {
		return nil
	}
	a.mu.Lock()
	client := a.pgClientVersion
	a.mu.Unlock()
	if client == "" {
		client = a.pgClient(ctx)
		a.mu.Lock()
		a.pgClientVersion = client
		a.mu.Unlock()
	}
	return &proto.PGFacts{Client: client, AllowInPlaceRestore: a.cfg.PostgreSQL.AllowInPlaceRestore}
}

// pgConnection returns the job's connection and the agent's config for PostgreSQL, or why it cannot run.
func (a *Agent) pgConnection(name string) (proto.PGConnection, error) {
	if a.cfg.PostgreSQL.BinDir == "" {
		return proto.PGConnection{}, errors.New("this agent has no PostgreSQL client: root here sets postgresql.bin_dir in agent.yaml")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hc == nil {
		return proto.PGConnection{}, errors.New("no config from the server yet")
	}
	c, ok := a.hc.PostgreSQL[name]
	if !ok {
		return proto.PGConnection{}, fmt.Errorf("the server sent no PostgreSQL connection named %q", name)
	}
	return c, nil
}

// pgPassFile writes the pgpass file one of a connection's users signs in with, from the password the server sent,
// and returns its path. A user with no password (peer authentication on a socket) gets an empty file.
func (a *Agent) pgPassFile(conn, kind, user string) (string, error) {
	dir := a.path("postgresql")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	pw, err := os.ReadFile(a.pgSecretPath(conn, kind))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	esc := func(s string) string { return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(s) }
	body := ""
	if len(pw) > 0 {
		body = "*:*:*:" + esc(user) + ":" + esc(string(pw)) + "\n"
	}
	f := filepath.Join(dir, conn+"-"+kind+".pgpass")
	if err := writeFileAtomic(f, []byte(body), 0o600); err != nil {
		return "", err
	}
	return f, nil
}

// pgSecretPath is where a connection's password is kept: in the credentials directory, root-only.
func (a *Agent) pgSecretPath(conn, kind string) string {
	name := "pg-" + conn + ".pass"
	if kind == pgRestoreUser {
		name = "pg-" + conn + "-restore.pass"
	}
	return filepath.Join(a.cfg.CredentialsDir, name)
}

// pgURL is the connection's URL with a database: libpq's URI, with no user or password in it.
func pgURL(raw, db string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.User = nil
	u.Path = "/" + db
	u.RawPath = ""
	return u.String(), nil
}

// pgEnv is the clean environment a PostgreSQL program runs with.
func (a *Agent) pgEnv(passFile string, extra ...string) []string {
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "HOME=" + a.cfg.StateDir, "PGPASSFILE=" + passFile,
		"PGAPPNAME=holdfast", "PGCONNECT_TIMEOUT=30"}
	return append(env, extra...)
}

// pgDumpCommand is what restic runs for a PostgreSQL job: pg_dump of one database in its custom format, uncompressed
// so restic's compression and deduplication work, or pg_dumpall of the roles and other globals, without their
// passwords. env -i drops restic's environment.
func (a *Agent) pgDumpCommand(j proto.JobConfig) ([]string, string, error) {
	c, err := a.pgConnection(j.PostgreSQL.Connection)
	if err != nil {
		return nil, "", err
	}
	pass, err := a.pgPassFile(j.PostgreSQL.Connection, pgUser, c.User)
	if err != nil {
		return nil, "", err
	}
	bin := a.cfg.PostgreSQL.BinDir
	argv := append([]string{"/usr/bin/env", "-i"}, a.pgEnv(pass)...)
	if j.PostgreSQL.Globals {
		u, err := pgURL(c.URL, "postgres")
		if err != nil {
			return nil, "", err
		}
		return append(argv, filepath.Join(bin, "pg_dumpall"), "--globals-only", "--no-role-passwords", "--no-password",
			"--username="+c.User, "--dbname="+u), "globals.sql", nil
	}
	u, err := pgURL(c.URL, j.PostgreSQL.Database)
	if err != nil {
		return nil, "", err
	}
	return append(argv, filepath.Join(bin, "pg_dump"), "--format=custom", "--compress=0", "--lock-wait-timeout=10min", "--no-password",
		"--username="+c.User, "--dbname="+u), j.PostgreSQL.Database + ".dump", nil
}

// psql runs one statement through psql as one of a connection's users, and returns its rows, tab-separated.
func (a *Agent) psql(ctx context.Context, connName string, c proto.PGConnection, kind, db, sql string) ([][]string, error) {
	user := c.User
	if kind == pgRestoreUser {
		user = c.RestoreUser
	}
	pass, err := a.pgPassFile(connName, kind, user)
	if err != nil {
		return nil, err
	}
	u, err := pgURL(c.URL, db)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, filepath.Join(a.cfg.PostgreSQL.BinDir, "psql"), "-X", "-A", "-t", "-q", "-F", "\t", "-v", "ON_ERROR_STOP=1",
		"--no-password", "--username="+user, "--dbname="+u, "-c", sql)
	cmd.Env = a.pgEnv(pass)
	var stdout bytes.Buffer
	stderr := &tailBufferAgent{max: 4096}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows, nil
}

// tailBufferAgent keeps the last max bytes written to it.
type tailBufferAgent struct {
	max int
	buf []byte
}

func (t *tailBufferAgent) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBufferAgent) String() string { return string(t.buf) }

/* ---------------- testing a connection ---------------- */

// runPGTest connects as the backup user, and as the restore user if there is one, and reports what it finds: the
// server's version, TLS, the databases with their sizes and owners, and what the restore user may do.
func (a *Agent) runPGTest(ctx context.Context, cmd proto.Command) {
	started := a.now().UTC()
	a.verifyCredentials()
	res := proto.PGTestResult{Connection: cmd.Connection}
	ev := proto.Event{Kind: proto.KindPGTest, CommandID: cmd.ID, Started: started, Revision: a.revision(), Detail: map[string]string{"connection": cmd.Connection}}
	finish := func(err error) {
		ev.Finished = a.now().UTC()
		ev.Status = proto.StatusSuccess
		if err != nil {
			ev.Status, ev.Error, res.Error = proto.StatusFailed, err.Error(), err.Error()
		}
		b, _ := json.Marshal(res)
		ev.Detail["result"] = string(b)
		a.spool(ev)
	}
	c, err := a.pgConnection(cmd.Connection)
	if err != nil {
		finish(err)
		return
	}
	res.Client = a.pgClient(ctx)
	if res.Client == "" {
		finish(fmt.Errorf("no pg_dump that runs in %s (postgresql.bin_dir)", a.cfg.PostgreSQL.BinDir))
		return
	}
	rows, err := a.psql(ctx, cmd.Connection, c, pgUser, "postgres",
		`SELECT split_part(current_setting('server_version'), ' ', 1), current_setting('server_version_num'),
		 COALESCE((SELECT version FROM pg_stat_ssl WHERE pid = pg_backend_pid() AND ssl), '')`)
	if err != nil {
		finish(fmt.Errorf("connecting as %s: %v", c.User, err))
		return
	}
	if len(rows) == 1 && len(rows[0]) == 3 {
		res.ServerVersion, res.TLS = rows[0][0], rows[0][2]
		res.ServerNum, _ = strconv.Atoi(rows[0][1])
	}
	rows, err = a.psql(ctx, cmd.Connection, c, pgUser, "postgres",
		`SELECT datname, CASE WHEN has_database_privilege(datname, 'CONNECT') THEN pg_database_size(datname) ELSE -1 END, pg_get_userbyid(datdba)
		 FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
	if err != nil {
		finish(fmt.Errorf("listing the databases as %s: %v", c.User, err))
		return
	}
	for _, r := range rows {
		if len(r) == 3 {
			n, _ := strconv.ParseInt(r[1], 10, 64)
			res.Databases = append(res.Databases, proto.PGDatabase{Name: r[0], Bytes: n, Owner: r[2]})
		}
	}
	if c.RestoreUser != "" {
		rr := &proto.PGRestoreRights{ActsAs: []string{}}
		rows, err := a.psql(ctx, cmd.Connection, c, pgRestoreUser, "postgres",
			`SELECT rolcreatedb OR rolsuper, COALESCE((SELECT string_agg(r.rolname, ',' ORDER BY r.rolname) FROM pg_roles r
			   WHERE r.rolname <> current_user AND r.rolname NOT LIKE 'pg\_%' AND pg_has_role(current_user, r.oid, 'MEMBER')), '')
			 FROM pg_roles WHERE rolname = current_user`)
		switch {
		case err != nil:
			rr.Error = fmt.Sprintf("connecting as %s: %v", c.RestoreUser, err)
		case len(rows) == 1 && len(rows[0]) == 2:
			rr.CreateDB = rows[0][0] == "t"
			if rows[0][1] != "" {
				rr.ActsAs = strings.Split(rows[0][1], ",")
			}
		}
		res.RestoreUser = rr
	}
	finish(nil)
}

/* ---------------- restoring a dump into a database ---------------- */

// pgIdent quotes a database or role name for SQL.
func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// pgLiteral quotes a string for SQL.
func pgLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// runPGRestore loads a dump into its server: a new database beside the original, or the original in place after a
// safety dump, in one transaction. It never drops a database. The result goes into ev, which the caller spools.
func (a *Agent) runPGRestore(ctx context.Context, cmd proto.Command, r restic.Runner, sn restic.Snapshot, ev *proto.Event) error {
	spec := cmd.Restore.PostgreSQL
	c, err := a.pgConnection(spec.Connection)
	if err != nil {
		return err
	}
	if c.RestoreUser == "" {
		return fmt.Errorf("connection %s has no restore user", spec.Connection)
	}
	if spec.InPlace && !a.cfg.PostgreSQL.AllowInPlaceRestore {
		return errors.New("refused by this agent: restores in place are not allowed (agent.yaml postgresql.allow_in_place_restore)")
	}
	target := spec.Name
	if spec.InPlace {
		target = spec.Database
	}
	if target == "" || len(target) > 63 || strings.ContainsAny(target, "\x00\"") {
		return fmt.Errorf("%q is not a database to restore into", target)
	}
	ev.Detail["connection"], ev.Detail["database"] = spec.Connection, target
	if spec.InPlace {
		ev.Detail["in_place"] = "true"
	} else {
		ev.Detail["new_database"] = target
	}
	exists := func(db string) (bool, string, error) {
		rows, err := a.psql(ctx, spec.Connection, c, pgRestoreUser, "postgres",
			"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = "+pgLiteral(db))
		if err != nil {
			return false, "", fmt.Errorf("connecting as %s: %v", c.RestoreUser, err)
		}
		if len(rows) == 0 {
			return false, "", nil
		}
		return true, rows[0][0], nil
	}
	targetExists, targetOwner, err := exists(target)
	if err != nil {
		return err
	}
	if !spec.InPlace && targetExists {
		return fmt.Errorf("database %s exists already: Holdfast restores into a new database only, or in place", target)
	}
	// The owner: the original's, so the copy belongs where the original does. If the original is gone, its roles may be
	// gone too: the copy, and everything in it, belongs to the restore user, without the original's grants.
	owner := targetOwner
	noOwner := false
	if !spec.InPlace {
		if ok, o, err := exists(spec.Database); err != nil {
			return err
		} else if ok {
			owner = o
		} else {
			noOwner = true
		}
	}
	if owner == "" {
		owner = c.RestoreUser
	}
	if noOwner {
		ev.Detail["no_owner"] = "true"
	}
	// The server's version: pg_restore's SQL leaves out what an older server does not know.
	serverNum := 0
	if rows, err := a.psql(ctx, spec.Connection, c, pgRestoreUser, "postgres", "SELECT current_setting('server_version_num')"); err != nil {
		return fmt.Errorf("connecting as %s: %v", c.RestoreUser, err)
	} else if len(rows) == 1 && len(rows[0]) == 1 {
		serverNum, _ = strconv.Atoi(rows[0][0])
	}
	if cmd.Restore.DryRun {
		// The dump must be one pg_restore reads: its table of contents, read through the pipe, writes nothing.
		if err := a.pgPipe(ctx, r, sn, spec, c, pgRestoreArgs{list: true}); err != nil {
			return fmt.Errorf("reading the dump with pg_restore --list: %v", err)
		}
		ev.Detail["owner"] = owner
		return nil
	}
	if spec.InPlace && targetExists {
		pre, err := a.pgSafetyDump(ctx, r, cmd, spec)
		if err != nil {
			return fmt.Errorf("the safety dump of %s failed, so nothing was restored: %v", target, err)
		}
		ev.Detail["pre_restore"] = pre
	}
	if !targetExists {
		if _, err := a.psql(ctx, spec.Connection, c, pgRestoreUser, "postgres", "CREATE DATABASE "+pgIdent(target)+" OWNER "+pgIdent(owner)); err != nil {
			return fmt.Errorf("creating database %s owned by %s: %v", target, owner, err)
		}
	}
	ev.Detail["owner"] = owner
	if err := a.pgPipe(ctx, r, sn, spec, c, pgRestoreArgs{db: target, clean: spec.InPlace && targetExists, noOwner: noOwner, serverNum: serverNum}); err != nil {
		if !targetExists {
			return fmt.Errorf("%v; database %s was made for this restore and is left as it is, empty or partly loaded: drop it, or restore again under another name", err, target)
		}
		return fmt.Errorf("%v; it ran in one transaction, so %s is as it was before", err, target)
	}
	if rows, err := a.psql(ctx, spec.Connection, c, pgRestoreUser, target,
		"SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')"); err == nil && len(rows) == 1 {
		ev.Detail["tables"] = rows[0][0]
	}
	return nil
}

type pgRestoreArgs struct {
	list      bool   // pg_restore --list: read the dump, write nothing
	db        string // the database to load into
	clean     bool   // in place: drop and make again what the dump holds, in the one transaction
	noOwner   bool   // the original is gone: no owners or grants from the dump, whose roles may be gone too
	serverNum int    // the server's version, such as 160003: what the client says that an older server does not know is left out
}

// pgPipe streams the dump out of restic into PostgreSQL: nothing is written to disk. For a dry run pg_restore reads the
// dump's table of contents (--list). To load it, pg_restore writes the dump as SQL and psql runs that in one
// transaction, stopping at the first error. That works on a server older than the client too: pg_restore's own session
// settings that the server does not know, such as transaction_timeout before PostgreSQL 17, are left out. pg_restore
// straight into the database sends them, and the load fails at once.
func (a *Agent) pgPipe(ctx context.Context, r restic.Runner, sn restic.Snapshot, spec *proto.PGRestore, c proto.PGConnection, o pgRestoreArgs) error {
	bin := a.cfg.PostgreSQL.BinDir
	cctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	args := []string{"--list"}
	if !o.list {
		args = []string{"--file=-"}
		if o.clean {
			args = append(args, "--clean", "--if-exists")
		}
		if o.noOwner {
			args = append(args, "--no-owner", "--no-acl")
		}
	}
	restore := exec.CommandContext(cctx, filepath.Join(bin, "pg_restore"), args...)
	restore.Env = a.pgEnv("")
	pr, pw := io.Pipe()
	restore.Stdin = pr
	restoreErr := &tailBufferAgent{max: 8192}
	restore.Stderr = restoreErr

	// The SQL goes to psql, as the restore user, in one transaction. A lock an application holds waits at most 5
	// minutes; then the transaction fails, and nothing changed.
	var load *exec.Cmd
	var sql io.ReadCloser
	var loadIn io.WriteCloser
	loadErr := &tailBufferAgent{max: 8192}
	if o.list {
		restore.Stdout = io.Discard
	} else {
		pass, err := a.pgPassFile(spec.Connection, pgRestoreUser, c.RestoreUser)
		if err != nil {
			return err
		}
		u, err := pgURL(c.URL, o.db)
		if err != nil {
			return err
		}
		if sql, err = restore.StdoutPipe(); err != nil {
			return err
		}
		load = exec.Command(filepath.Join(bin, "psql"), "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "--no-password",
			"--username="+c.RestoreUser, "--dbname="+u, "--file=-")
		load.Env = a.pgEnv(pass, "PGOPTIONS=-c lock_timeout=300000")
		load.Stdout, load.Stderr = io.Discard, loadErr
		if loadIn, err = load.StdinPipe(); err != nil {
			return err
		}
		if err := load.Start(); err != nil {
			return err
		}
	}
	if err := restore.Start(); err != nil {
		if load != nil {
			_ = load.Process.Kill()
			_ = load.Wait()
		}
		return err
	}

	// The SQL is copied to psql. psql commits only once pg_restore has finished well: if pg_restore fails, psql is
	// stopped first, so a dump read only partly never commits. If psql stops (an error), pg_restore's output is closed.
	copied := make(chan bool, 1) // whether psql stopped reading
	if sql != nil {
		go func() {
			werr := pgCopySQL(loadIn, sql, o.serverNum)
			if werr != nil {
				_ = sql.Close()
			}
			copied <- werr != nil
		}()
	} else {
		copied <- false
	}
	// pg_restore may finish before the dump does: --list reads only the table of contents. Then nothing reads the pipe
	// any more, and restic's writes must fail rather than wait.
	waited := make(chan error, 1)
	var loadGone bool
	go func() {
		loadGone = <-copied
		err := restore.Wait()
		_ = pr.CloseWithError(errPGRestoreExited)
		if load != nil {
			if err == nil {
				_ = loadIn.Close() // the end of the SQL: psql commits
			} else {
				_ = load.Process.Kill() // no COMMIT: the server rolls the transaction back
			}
		}
		waited <- err
	}()
	dumpErr := r.Dump(cctx, sn.ID, "/"+spec.File, pw)
	_ = pw.CloseWithError(dumpErr)
	restoreWaitErr := <-waited
	var loadWaitErr error
	if load != nil {
		loadWaitErr = load.Wait()
	}
	tail := func(b *tailBufferAgent, err error) string {
		if msg := strings.TrimSpace(b.String()); msg != "" {
			return msg
		}
		return err.Error()
	}
	switch {
	case loadGone && loadWaitErr != nil:
		return fmt.Errorf("loading the dump: %s", tail(loadErr, loadWaitErr))
	case restoreWaitErr != nil:
		return fmt.Errorf("pg_restore: %s", tail(restoreErr, restoreWaitErr))
	case loadWaitErr != nil:
		return fmt.Errorf("loading the dump: %s", tail(loadErr, loadWaitErr))
	case dumpErr != nil && !o.list:
		return fmt.Errorf("reading the dump from the repository: %v", dumpErr)
	}
	return nil
}

// pgCopySQL copies pg_restore's SQL to psql, line by line, leaving out the session settings a server older than
// PostgreSQL 17 does not know. It returns the error of a write: psql stopped reading.
func pgCopySQL(dst io.Writer, src io.Reader, serverNum int) error {
	br := bufio.NewReaderSize(src, 1<<20)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 && !(serverNum > 0 && serverNum < 170000 && bytes.Equal(bytes.TrimSpace(line), []byte("SET transaction_timeout = 0;"))) {
			if _, err := dst.Write(line); err != nil {
				return err
			}
		}
		if rerr != nil {
			return nil
		}
	}
}

// errPGRestoreExited is what restic's writes into the pipe get once pg_restore has exited.
var errPGRestoreExited = errors.New("pg_restore has exited")

// pgSafetyDump backs up the database an in-place restore replaces, as a snapshot of the job tagged pre-restore, and
// reports it as a pre_restore event. It returns the snapshot's ID.
func (a *Agent) pgSafetyDump(ctx context.Context, r restic.Runner, cmd proto.Command, spec *proto.PGRestore) (string, error) {
	j := proto.JobConfig{ID: cmd.Restore.JobID, PostgreSQL: &proto.PGJob{Connection: spec.Connection, Database: spec.Database}}
	argv, file, err := a.pgDumpCommand(j)
	if err != nil {
		return "", err
	}
	tags := []string{"pre-restore=" + cmd.ID}
	if j.ID != "" {
		tags = append([]string{"job=" + j.ID}, tags...)
	}
	started := a.now().UTC()
	res, err := r.Backup(ctx, restic.BackupOptions{Host: a.cfg.FQDN, Tags: tags, StdinCommand: argv, StdinFilename: file})
	if err != nil {
		return "", err
	}
	a.spool(proto.Event{Kind: proto.KindPreRestore, JobID: j.ID, CommandID: cmd.ID, Status: proto.StatusSuccess, Started: started,
		Finished: a.now().UTC(), SnapshotID: res.SnapshotID, BytesAdded: res.DataAdded, TotalFiles: res.TotalFilesProcessed,
		TotalBytes: res.TotalBytesProcessed, Detail: map[string]string{"restore_of": cmd.Restore.Snapshot, "database": spec.Database}})
	return res.SnapshotID, nil
}

/* ---------------- credentials ---------------- */

// writePGSecrets keeps the connections' passwords in root-only files, and removes those of connections that are gone.
func (a *Agent) writePGSecrets(secrets map[string]proto.PGSecret) error {
	keep := map[string]bool{}
	for conn, s := range secrets {
		for kind, pw := range map[string]string{pgUser: s.Password, pgRestoreUser: s.RestorePassword} {
			p := a.pgSecretPath(conn, kind)
			if pw == "" {
				_ = os.Remove(p)
				continue
			}
			if err := writeFileAtomic(p, []byte(pw), 0o600); err != nil {
				return err
			}
			keep[filepath.Base(p)] = true
		}
	}
	entries, _ := os.ReadDir(a.cfg.CredentialsDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pg-") && strings.HasSuffix(e.Name(), ".pass") && !keep[e.Name()] {
			_ = os.Remove(filepath.Join(a.cfg.CredentialsDir, e.Name()))
		}
	}
	return nil
}

// pgSecretsDigest is what the credentials' digest adds for the connections' passwords: the files as they are now, in
// order. Without any, it adds nothing, so an agent with no connection keeps the digest it always had.
func (a *Agent) pgSecretsDigest() []byte {
	entries, _ := os.ReadDir(a.cfg.CredentialsDir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pg-") && strings.HasSuffix(e.Name(), ".pass") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b []byte
	for _, n := range names {
		pw, _ := os.ReadFile(filepath.Join(a.cfg.CredentialsDir, n))
		b = append(b, []byte("\n"+n+"\n")...)
		b = append(b, pw...)
	}
	return b
}
