package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/checks"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// PostgreSQL connections and jobs (decision 055). A connection is set centrally: its address and users are part of
// the config and of every revision, and its passwords are sealed with the credentials key, as repository passwords
// are, and sent only to the agent. The agent connects, runs pg_dump through restic, and restores; it reports what it
// finds when a connection is tested.

// ConnectionView is one PostgreSQL connection as the API shows it: never a password, only whether one is set.
type ConnectionView struct {
	Name               string          `json:"name"`
	URL                string          `json:"url"`
	User               string          `json:"user"`
	PasswordSet        bool            `json:"password_set"`
	RestoreUser        string          `json:"restore_user,omitempty"`
	RestorePasswordSet bool            `json:"restore_password_set,omitempty"`
	UsedBy             []string        `json:"used_by"`
	LastTest           *ConnectionTest `json:"last_test,omitempty"`
}

// ConnectionTest is the newest test the agent reported for a connection.
type ConnectionTest struct {
	At      time.Time           `json:"at"`
	OK      bool                `json:"ok"`
	Command string              `json:"command,omitempty"`
	Error   string              `json:"error,omitempty"`
	Result  *proto.PGTestResult `json:"result,omitempty"`
}

// pgMajor is a PostgreSQL version's major number: 16 for "16.3", 18 for "18beta1"; 0 if there is none.
func pgMajor(v string) int {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(v[:i])
	return n
}

// connectionViews are an agent's connections, with their last tests.
func (s *Server) connectionViews(ctx context.Context, q *store.Q, fqdn string, h config.Host) ([]ConnectionView, error) {
	out := []ConnectionView{}
	if len(h.PostgreSQL) == 0 {
		return out, nil
	}
	pws, err := q.PGPasswords(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	tests, err := q.LatestPGTests(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(h.PostgreSQL))
	for n := range h.PostgreSQL {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := h.PostgreSQL[n]
		v := ConnectionView{Name: n, URL: c.URL, User: c.User, RestoreUser: c.RestoreUser, UsedBy: []string{}}
		for _, p := range pws {
			if p.Connection == n {
				v.PasswordSet = v.PasswordSet || p.Kind == store.PGBackup
				v.RestorePasswordSet = v.RestorePasswordSet || p.Kind == store.PGRestore
			}
		}
		for id, j := range h.Jobs {
			if j.PostgreSQL != nil && j.PostgreSQL.Connection == n {
				v.UsedBy = append(v.UsedBy, id)
			}
		}
		sort.Strings(v.UsedBy)
		if e, ok := tests[n]; ok {
			t := &ConnectionTest{At: e.Finished, OK: e.Status == proto.StatusSuccess, Command: e.CommandID, Error: e.Error}
			t.Result = store.PGTestDetail[proto.PGTestResult](e)
			v.LastTest = t
		}
		out = append(out, v)
	}
	return out, nil
}

// pgSecrets opens an agent's PostgreSQL passwords, for its credentials.
func (s *Server) pgSecrets(ctx context.Context, q *store.Q, fqdn string) (map[string]proto.PGSecret, error) {
	pws, err := q.PGPasswords(ctx, fqdn)
	if err != nil || len(pws) == 0 {
		return nil, err
	}
	out := map[string]proto.PGSecret{}
	for _, p := range pws {
		pw, err := s.seal.Open(p.Sealed, pgAAD(fqdn, p.Connection, p.Kind))
		if err != nil {
			return nil, fmt.Errorf("the password of connection %s: %w", p.Connection, err)
		}
		sec := out[p.Connection]
		if p.Kind == store.PGRestore {
			sec.RestorePassword = pw
		} else {
			sec.Password = pw
		}
		out[p.Connection] = sec
	}
	return out, nil
}

// pgAAD binds a sealed password to its agent, connection and kind, so a row moved elsewhere does not open.
func pgAAD(fqdn, conn, kind string) string { return "pg:" + fqdn + "/" + conn + "/" + kind }

/* ---------------- setting, removing and testing a connection ---------------- */

// SetConnectionInput sets a connection (PUT /api/v1/agents/{fqdn}/postgresql/{name}). A password that is not given
// keeps the one stored; one given empty removes it, for peer authentication on a socket.
type SetConnectionInput struct {
	Base            *int64  `json:"base_revision,omitempty"`
	Message         string  `json:"message"`
	URL             string  `json:"url"`
	User            string  `json:"user"`
	Password        *string `json:"password,omitempty"`
	RestoreUser     string  `json:"restore_user,omitempty"`
	RestorePassword *string `json:"restore_password,omitempty"`
}

// SetConnectionResult is the saved revision, and the test the save queued.
type SetConnectionResult struct {
	Revision int64  `json:"revision"`
	Test     string `json:"test_command"`
}

// SetConnection saves a connection as a new revision, its passwords sealed apart from it, and has the agent test it.
func (s *Server) SetConnection(ctx context.Context, a Actor, fqdn, name string, in SetConnectionInput) (SetConnectionResult, error) {
	if s.set.IsVault() {
		return SetConnectionResult{}, errVaultHosts
	}
	if err := config.ValidConnectionName(name); err != nil {
		return SetConnectionResult{}, &ValidationError{config.Problems{{Path: "name", Message: err.Error()}}}
	}
	conn := config.PGConnection{URL: strings.TrimSpace(in.URL), User: strings.TrimSpace(in.User), RestoreUser: strings.TrimSpace(in.RestoreUser)}
	if p := conn.Validate(); len(p) > 0 {
		probs := config.Problems{}
		for _, m := range p {
			probs = append(probs, config.Problem{Path: "connection", Message: m})
		}
		return SetConnectionResult{}, &ValidationError{probs}
	}
	if in.RestorePassword != nil && *in.RestorePassword != "" && conn.RestoreUser == "" {
		return SetConnectionResult{}, &ValidationError{config.Problems{{Path: "restore_password", Message: "a restore password goes with a restore user"}}}
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = fmt.Sprintf("Set PostgreSQL connection %s on %s", name, fqdn)
	}
	res := SetConnectionResult{Test: newID()}
	now := s.now()
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		if in.Base != nil && *in.Base != head.Number {
			return ErrConflict
		}
		h, ok := head.Config.Hosts[fqdn]
		if !ok {
			return fmt.Errorf("agent %s: %w", fqdn, ErrNotFound)
		}
		_, existed := h.PostgreSQL[name]
		doc := head.Config.Clone()
		h = doc.Hosts[fqdn]
		if h.PostgreSQL == nil {
			h.PostgreSQL = map[string]config.PGConnection{}
		}
		h.PostgreSQL[name] = conn
		doc.Hosts[fqdn] = h
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message,
			Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		res.Revision = rev.Number
		changed := false
		put := func(kind string, pw *string) error {
			switch {
			case pw == nil:
				return nil
			case *pw == "":
				gone, err := q.DeletePGPassword(ctx, fqdn, name, kind)
				changed = changed || gone
				return err
			}
			changed = true
			return q.PutPGPassword(ctx, store.PGPassword{FQDN: fqdn, Connection: name, Kind: kind, Sealed: s.seal.Seal(*pw, pgAAD(fqdn, name, kind)), UpdatedAt: now})
		}
		if err := put(store.PGBackup, in.Password); err != nil {
			return err
		}
		restorePW := in.RestorePassword
		if conn.RestoreUser == "" {
			empty := ""
			restorePW = &empty // no restore user: no restore password either
		}
		if err := put(store.PGRestore, restorePW); err != nil {
			return err
		}
		if changed {
			if err := q.BumpCredentials(ctx, fqdn); err != nil {
				return err
			}
		}
		action := "postgresql.connection_add"
		if existed {
			action = "postgresql.connection_change"
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: action, FQDN: fqdn, Revision: rev.Number,
			Detail: map[string]any{"connection": name, "url": conn.URL, "user": conn.User, "restore_user": conn.RestoreUser,
				"password_changed": in.Password != nil, "restore_password_changed": in.RestorePassword != nil, "message": in.Message}}); err != nil {
			return err
		}
		// The agent tests it at once, so the page shows what it reaches, or why it cannot.
		if err := s.enqueuePGTest(ctx, q, a, fqdn, name, res.Test); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return res, err
}

// RemoveConnection removes a connection no job uses, and its passwords.
func (s *Server) RemoveConnection(ctx context.Context, a Actor, fqdn, name, message string) (int64, error) {
	var revNo int64
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		h, ok := head.Config.Hosts[fqdn]
		if !ok {
			return fmt.Errorf("agent %s: %w", fqdn, ErrNotFound)
		}
		if _, ok := h.PostgreSQL[name]; !ok {
			return fmt.Errorf("connection %s on %s: %w", name, fqdn, ErrNotFound)
		}
		for id, j := range h.Jobs {
			if j.PostgreSQL != nil && j.PostgreSQL.Connection == name {
				return &ValidationError{config.Problems{{Path: "connection", Message: fmt.Sprintf("job %s uses connection %s: remove the job first", id, name)}}}
			}
		}
		doc := head.Config.Clone()
		h = doc.Hosts[fqdn]
		delete(h.PostgreSQL, name)
		doc.Hosts[fqdn] = h
		if strings.TrimSpace(message) == "" {
			message = fmt.Sprintf("Remove PostgreSQL connection %s from %s", name, fqdn)
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: message,
			Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		revNo = rev.Number
		if err := q.DeletePGPasswords(ctx, fqdn, name); err != nil {
			return err
		}
		if err := q.BumpCredentials(ctx, fqdn); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "postgresql.connection_remove", FQDN: fqdn, Revision: rev.Number,
			Detail: map[string]any{"connection": name, "message": message}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return revNo, err
}

// pgTestJoin is how recent a test of a connection still unanswered is joined by Test connection, rather than queueing
// another one: the result of one would hide the other's from whoever waits for it.
const pgTestJoin = 5 * time.Minute

// TestConnection has the agent connect, as it would to back up, and report what it finds. A test of the connection
// asked moments ago and not answered yet, such as the one saving it queued, is the test: its command is returned.
func (s *Server) TestConnection(ctx context.Context, a Actor, fqdn, name string) (string, error) {
	id := newID()
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		if _, ok := head.Config.Hosts[fqdn].PostgreSQL[name]; !ok {
			return fmt.Errorf("connection %s on %s: %w", name, fqdn, ErrNotFound)
		}
		now := s.now()
		if open, err := q.UnfinishedPGTest(ctx, fqdn, name, now.Add(-pgTestJoin), now); err != nil {
			return err
		} else if open != "" {
			id = open
			return nil
		}
		if err := s.enqueuePGTest(ctx, q, a, fqdn, name, id); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return id, err
}

func (s *Server) enqueuePGTest(ctx context.Context, q *store.Q, a Actor, fqdn, name, id string) error {
	payload, _ := json.Marshal(map[string]string{"connection": name})
	expires := s.now().Add(time.Hour)
	if err := q.EnqueueCommand(ctx, store.Command{ID: id, FQDN: fqdn, Kind: proto.CommandPGTest, CreatedAt: s.now(), CreatedBy: a.Name, Payload: payload,
		ExpiresAt: &expires}); err != nil {
		return err
	}
	return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "postgresql.connection_test", FQDN: fqdn,
		Detail: map[string]any{"connection": name, "command": id}})
}

/* ---------------- the API ---------------- */

func (s *Server) apiConnections(w http.ResponseWriter, r *http.Request) {
	fqdn := r.PathValue("fqdn")
	q := s.st.Q()
	head, err := s.head(r.Context(), q)
	if err != nil {
		apiError(w, err)
		return
	}
	h, ok := head.Config.Hosts[fqdn]
	if !ok {
		apiError(w, fmt.Errorf("agent %s: %w", fqdn, ErrNotFound))
		return
	}
	v, err := s.connectionViews(r.Context(), q, fqdn, h)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiSetConnection(w http.ResponseWriter, r *http.Request) {
	var in SetConnectionInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.SetConnection(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("name"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiRemoveConnection(w http.ResponseWriter, r *http.Request) {
	rev, err := s.RemoveConnection(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("name"), r.URL.Query().Get("message"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
}

func (s *Server) apiTestConnection(w http.ResponseWriter, r *http.Request) {
	id, err := s.TestConnection(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("name"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"command": id})
}

/* ---------------- checks on a PostgreSQL job ---------------- */

// pgFindings are the P checks on a PostgreSQL job (spec §4.5): P001 an agent's pg_dump older than the server, P003 a
// database the last test did not find, P004 an agent that cannot run PostgreSQL jobs, P005 a connection whose last
// test failed or that was never tested.
func (s *Server) pgFindings(ctx context.Context, fqdn string, j config.Job) []checks.Finding {
	p := j.PostgreSQL
	if p == nil {
		return nil
	}
	var fs []checks.Finding
	add := func(code, sev, path, format string, a ...any) {
		fs = append(fs, checks.Finding{Code: code, Severity: sev, Path: path, Message: fmt.Sprintf(format, a...)})
	}
	q := s.st.Q()
	ag, agErr := q.GetAgent(ctx, fqdn)
	if agErr == nil {
		switch {
		case (proto.Facts{Protocol: ag.Protocol}).AgentProtocol() < proto.ProtocolPostgreSQL:
			add("P004", checks.Error, "engine", "the agent on %s (%s) is too old for PostgreSQL jobs: upgrade holdfast-agent there first", fqdn, ag.AgentVersion)
		case ag.PostgreSQL == nil || ag.PostgreSQL.Client == "":
			add("P004", checks.Error, "engine", "the agent on %s has no PostgreSQL client: root there sets postgresql.bin_dir in its agent.yaml to a directory with pg_dump", fqdn)
		}
	}
	tests, err := q.LatestPGTests(ctx, fqdn)
	if err != nil {
		s.log.Warn("reading the tests of PostgreSQL connections", "host", fqdn, "err", err)
		return fs
	}
	e, tested := tests[p.Connection]
	var res *proto.PGTestResult
	if tested {
		res = store.PGTestDetail[proto.PGTestResult](e)
	}
	switch {
	case !tested:
		add("P005", checks.Warning, "postgresql.connection", "connection %s has not been tested yet, so it is not known to work: test it on the agent's page (holdfast agent connection test %s %s)", p.Connection, fqdn, p.Connection)
	case e.Status != proto.StatusSuccess:
		add("P005", checks.Warning, "postgresql.connection", "the last test of connection %s failed: %s. Every backup fails until it works", p.Connection, e.Error)
	}
	if res != nil && res.ServerVersion != "" {
		client := res.Client
		if agErr == nil && ag.PostgreSQL != nil && ag.PostgreSQL.Client != "" {
			client = ag.PostgreSQL.Client
		}
		if c, sv := pgMajor(client), pgMajor(res.ServerVersion); c > 0 && sv > 0 && c < sv {
			add("P001", checks.Error, "postgresql.connection", "pg_dump %s on %s is older than the server, PostgreSQL %s: pg_dump must be %d or newer. Root there points postgresql.bin_dir at a newer client", client, fqdn, res.ServerVersion, sv)
		}
		if !p.Globals && e.Status == proto.StatusSuccess {
			found := false
			for _, d := range res.Databases {
				found = found || d.Name == p.Database
			}
			if !found {
				add("P003", checks.Warning, "postgresql.database", "database %s was not on %s at its last test, %s", p.Database, p.Connection, e.Finished.UTC().Format("2006-01-02 15:04 UTC"))
			}
		}
	}
	return fs
}

// pgProblems are an agent's PostgreSQL problems: a connection whose last test failed, and P001.
func pgProblems(v HostView) []Problem {
	var out []Problem
	for _, c := range v.PostgreSQL {
		t := c.LastTest
		switch {
		case t == nil:
		case !t.OK:
			out = append(out, Problem{Code: "postgresql", Severity: "warning", Message: fmt.Sprintf("PostgreSQL connection %s: the last test failed: %s", c.Name, t.Error)})
		case t.Result != nil && v.Agent != nil && v.Agent.PostgreSQL != nil:
			if cl, sv := pgMajor(v.Agent.PostgreSQL.Client), pgMajor(t.Result.ServerVersion); cl > 0 && sv > 0 && cl < sv {
				out = append(out, Problem{Code: "P001", Severity: "warning", Message: fmt.Sprintf("PostgreSQL connection %s: pg_dump %s is older than the server, PostgreSQL %s; no job can use it until postgresql.bin_dir in the agent's agent.yaml names a client of version %d or newer",
					c.Name, v.Agent.PostgreSQL.Client, t.Result.ServerVersion, sv)})
			}
		}
	}
	return out
}

/* ---------------- restores into a database ---------------- */

// pgRestoreName is the database a dump restores into beside the original: <database>-restore-<date>-<time>, the dump's
// time in the server's zone, cut to PostgreSQL's 63 bytes.
func (s *Server) pgRestoreName(db string, t time.Time) string {
	suffix := "-restore-" + t.In(s.zone).Format("20060102-1504")
	if len(db)+len(suffix) > 63 {
		db = db[:63-len(suffix)]
	}
	return db + suffix
}

// pgRestoreSpec checks a restore of a PostgreSQL job's dump, and makes its spec: into a new database beside the
// original, in place (an admin's, with the database's name typed, where the agent allows it), or as a file.
func (s *Server) pgRestoreSpec(ctx context.Context, a Actor, fqdn string, j config.Job, sn store.Snapshot, in *RestoreInput, ag store.Agent, agErr error) (*proto.PGRestore, error) {
	p := j.PostgreSQL
	file := p.Database + ".dump"
	if p.Globals {
		file = "globals.sql"
	}
	switch in.Target {
	case proto.TargetNew, proto.TargetDirectory:
		return nil, nil // the dump file, as any file
	case proto.TargetDatabase, proto.TargetInPlace:
	default:
		return nil, &ValidationError{config.Problems{{Path: "target", Message: "a dump restores into a new database (database), in place, or as the file (new)"}}}
	}
	if p.Globals {
		return nil, &ValidationError{config.Problems{{Path: "target", Message: "roles and globals are restored as the file only: Holdfast never creates roles on a server"}}}
	}
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return nil, err
	}
	conn, ok := head.Config.Hosts[fqdn].PostgreSQL[p.Connection]
	switch {
	case !ok:
		return nil, &ValidationError{config.Problems{{Path: "target", Message: fmt.Sprintf("connection %s is gone from %s: restore the dump as a file", p.Connection, fqdn)}}}
	case conn.RestoreUser == "":
		return nil, &ValidationError{config.Problems{{Path: "target", Message: fmt.Sprintf("connection %s has no restore user: restore the dump as a file, or give the connection one", p.Connection)}}}
	case len(in.Include) > 0 || len(in.Exclude) > 0 || in.Overwrite != "" || in.Directory != "":
		return nil, &ValidationError{config.Problems{{Path: "target", Message: "a restore into a database takes the whole dump: no include, exclude, overwrite or directory"}}}
	case in.ToHost != "":
		return nil, &ValidationError{config.Problems{{Path: "to_host", Message: "a dump restores onto its own server only"}}}
	}
	if agErr == nil && (proto.Facts{Protocol: ag.Protocol}).AgentProtocol() < proto.ProtocolPostgreSQL {
		return nil, fmt.Errorf("%s's agent does not know restores into a database: upgrade holdfast-agent there first: %w", fqdn, ErrConflict)
	}
	spec := &proto.PGRestore{Connection: p.Connection, Database: p.Database, File: file}
	if in.Target == proto.TargetInPlace {
		if err := s.allowed(ctx, a, access.RestoreInPlace, fqdn); err != nil {
			return nil, err
		}
		if in.Confirm != p.Database {
			return nil, &ValidationError{config.Problems{{Path: "confirm", Message: fmt.Sprintf("a restore in place replaces database %s: confirm it by giving its name", p.Database)}}}
		}
		if agErr == nil && (ag.PostgreSQL == nil || !ag.PostgreSQL.AllowInPlaceRestore) {
			return nil, &ValidationError{config.Problems{{Path: "target", Message: fqdn + " does not allow restores in place; root on it can allow them in agent.yaml (postgresql.allow_in_place_restore)"}}}
		}
		spec.InPlace = true
	} else {
		spec.Name = s.pgRestoreName(p.Database, sn.Time)
	}
	in.Target = proto.TargetDatabase
	return spec, nil
}
