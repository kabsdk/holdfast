package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Login and roles, against a real Authelia (plan-m4 §6, decisions 033 and 034):
//   - the UI sends a browser to the provider, and back; an admin sees every host, wes only web01;
//   - holdfast login uses the device flow, approved in a browser, and later commands use that session;
//   - each role gets exactly what spec §6.2 says, and a host without a binding does not exist.
func TestLoginAndRoles(t *testing.T) {
	src := smallSource(t)
	e := newEnvWith(t, envOpts{OIDC: true})
	const web, db = "web01.example.com", "db01.example.com"
	e.startAgent(web, e.addHost(web, "system", "--path", src, "--daily", "02:00"))
	e.waitProgress(web, "first_backup_done", 2*time.Minute)
	e.addHost(db, "system", "--path", src, "--daily", "02:00")
	bindings := filepath.Join(e.dir, "bindings.json")
	must(t, os.WriteFile(bindings, []byte(`[
		{"group":"holdfast-admins","role":"admin","hosts":["*"]},
		{"group":"holdfast-editors","role":"editor","hosts":["*"]},
		{"group":"holdfast-viewers","role":"viewer","hosts":["*"]},
		{"group":"web-team","role":"viewer","hosts":["web01.example.com"]}]`), 0o600))
	e.cli("access", "set", "--file", bindings, "-m", "web-team sees web01")

	// The browser: an admin sees both hosts; wes, in a fresh browser, only web01.
	ada := newBrowser(t)
	uiLogin(t, ada, "ada")
	run(t, ada, "the fleet", chromedp.Click(`.chip:nth-of-type(2)`, chromedp.ByQuery)) // All hosts
	if fleet := textOf(t, ada, "main table"); !strings.Contains(fleet, web) || !strings.Contains(fleet, db) {
		t.Fatalf("ada must see both hosts:\n%s", fleet)
	}
	if nav := textOf(t, ada, "nav.side"); !strings.Contains(nav, "Access") {
		t.Fatalf("an admin everywhere gets the Access page: %s", nav)
	}
	wes := newBrowser(t)
	uiLogin(t, wes, "wes")
	run(t, wes, "the fleet", chromedp.Click(`.chip:nth-of-type(2)`, chromedp.ByQuery))
	if fleet := textOf(t, wes, "main table"); !strings.Contains(fleet, web) || strings.Contains(fleet, db) {
		t.Fatalf("wes must see web01 only:\n%s", fleet)
	}
	if nav := textOf(t, wes, "nav.side"); strings.Contains(nav, "Access") {
		t.Fatalf("wes gets no Access page: %s", nav)
	}

	// The CLI: each role through its own session.
	ed := e.cliLogin(t, "ed")
	vi := e.cliLogin(t, "vi")
	wesCLI := e.cliLogin(t, "wes")
	var me struct {
		Name   string              `json:"name"`
		Source string              `json:"source"`
		Hosts  map[string][]string `json:"hosts"`
	}
	out, err := e.cliAs(wesCLI, "whoami")
	if err != nil || json.Unmarshal(out, &me) != nil || me.Name != "wes" || me.Source != "cli" || len(me.Hosts) != 1 || me.Hosts[web] == nil {
		t.Fatalf("whoami as wes: %v %s", err, out)
	}
	snaps := e.snapshotIDs(web)
	for _, tc := range []struct {
		name  string
		as    []string
		args  []string
		ok    bool
		error string
	}{
		{"wes cannot see db01", wesCLI, []string{"host", "show", db}, false, "not found"},
		{"wes sees web01", wesCLI, []string{"host", "show", web}, true, ""},
		{"a viewer sees no file names", vi, []string{"snapshot", "files", web, snaps[0]}, false, "browse"},
		{"an editor does", ed, []string{"snapshot", "files", web, snaps[0]}, true, ""},
		{"a viewer cannot back up", vi, []string{"job", "run", web, "system"}, false, "backup.run"},
		{"an editor can", ed, []string{"job", "run", web, "system"}, true, ""},
		{"an editor cannot restore in place", ed, []string{"restore", web, snaps[0], "--in-place"}, false, "restore.in_place"},
		{"an editor cannot see the bindings", ed, []string{"access", "list"}, false, "every host"},
		{"an editor cannot Prune now", ed, []string{"prune-now", web}, false, "retention.cut"},
	} {
		out, err := e.cliAs(tc.as, tc.args...)
		if tc.ok != (err == nil) || (!tc.ok && !strings.Contains(string(out), tc.error)) {
			t.Errorf("%s: %v\n%s", tc.name, err, out)
		}
	}
	// Who did it is in the audit log, as the provider's username.
	var audit []auditRow
	must(t, json.Unmarshal(e.cli("audit", "--host", web), &audit))
	found := false
	for _, a := range audit {
		found = found || (a.Action == "backup.run_now" && a.Actor == "ed")
	}
	if !found {
		t.Errorf("ed's backup is not in the audit log as ed")
	}
	// Logout ends the CLI session.
	if out, err := e.cliAs(ed, "logout"); err != nil {
		t.Fatalf("logout: %v %s", err, out)
	}
	if out, err := e.cliAs(ed, "--server", uiURL, "whoami"); err == nil || !strings.Contains(string(out), "not logged in") {
		t.Fatalf("after logout: %v %s", err, out)
	}
}

// snapshotIDs lists a host's snapshot IDs through the root socket.
func (e *env) snapshotIDs(fqdn string) []string {
	e.t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	must(e.t, json.Unmarshal(e.cli("snapshot", "list", fqdn, "--refresh"), &rows))
	if len(rows) == 0 {
		e.t.Fatalf("%s has no snapshots", fqdn)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}
