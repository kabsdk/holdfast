package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

type snapRow struct {
	ID               string     `json:"id"`
	Original         string     `json:"original"`
	MadeByHoldfast   bool       `json:"made_by_holdfast"`
	GoneAt           *time.Time `json:"gone_at"`
	DeletedInPrimary *time.Time `json:"deleted_in_primary_at"`
}

// snaps lists a host's snapshots; with --offsite first, its off-site copies.
func (e *env) snaps(fqdn string, flags ...string) []snapRow {
	e.t.Helper()
	var rows []snapRow
	args := []string{"snapshot", "list", fqdn}
	if len(flags) > 0 && flags[0] == "--offsite" {
		args, flags = append([]string{"--offsite"}, args...), flags[1:]
	}
	out, err := e.cliErr(append(args, flags...)...)
	if err == nil {
		_ = json.Unmarshal(out, &rows)
	}
	return rows
}

// copyOf returns the off-site copy of a primary snapshot, if there is one now.
func copyOf(rows []snapRow, original string) (snapRow, bool) {
	for _, r := range rows {
		if r.Original == original && r.GoneAt == nil {
			return r, true
		}
	}
	return snapRow{}, false
}

func presentIDs(rows []snapRow) []string {
	var out []string
	for _, r := range rows {
		if r.GoneAt == nil {
			out = append(out, r.ID)
		}
	}
	slices.Sort(out)
	return out
}

func sortedIDs(xs ...string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return out
}

// offsiteView is the host page's off-site part.
type offsiteView struct {
	Copies  int `json:"copies"`
	Pending int `json:"pending"`
	Jobs    []struct {
		Excluded bool `json:"excluded"`
	} `json:"jobs"`
}

func (e *env) offsiteOf(fqdn string) *offsiteView {
	e.t.Helper()
	var h struct {
		Offsite *offsiteView `json:"offsite"`
	}
	out, err := e.cliErr("agent", "show", fqdn)
	if err == nil {
		_ = json.Unmarshal(out, &h)
	}
	return h.Offsite
}

// TestOffsite is decisions 027, 031, 039 and 041 end to end: one holdfast-server with offsite: in its settings, an
// agent, two buckets, real restic.
//   - The server copies each new snapshot off-site, which keeps its host, tags and original; the copies count as made
//     by Holdfast; the off-site retention starts as primary's; the host's page shows what was copied.
//   - The hold: while the off-site storage fails, the daily run keeps what is not copied yet.
//   - Deleted in primary, and Delete here too; a copy deleted off-site is not made again.
//   - Exclude and Include.
//   - A restore on the host from its off-site copy, through the gateway, which lets it read that copy only then.
func TestOffsite(t *testing.T) {
	src := smallSource(t)
	e := newEnvWith(t, envOpts{Offsite: true, UI: true})
	const fqdn = "web01.example.com"
	agent := e.startAgent(fqdn, e.addHost(fqdn, "data", "--path", src, "--daily", "02:00", "--keep-last", "1"))
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	backups := 1
	backup := func() string {
		t.Helper()
		must(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d.txt", backups+1)), []byte(fmt.Sprintf("version %d\n", backups+1)), 0o644))
		e.cli("job", "run", fqdn, "data")
		backups++
		waitStatus(t, e, fqdn, "data", "success", backups)
		return e.latestSnapshot(fqdn, "data")
	}
	s1 := e.latestSnapshot(fqdn, "data")
	s2 := backup()

	// Each copy is a run in the off-site history, recorded just after the off-site catalogue sees it: then it counts
	// as made by Holdfast.
	waitFor(t, "the first copies", 3*time.Minute, func() bool {
		rows := e.snaps(fqdn, "--offsite")
		c1, ok1 := copyOf(rows, s1)
		c2, ok2 := copyOf(rows, s2)
		return ok1 && ok2 && c1.MadeByHoldfast && c2.MadeByHoldfast
	})
	// Site B measures what the job's copies take, as site A measures its own, once the first copy is there.
	waitFor(t, "the off-site job's stored size", 3*time.Minute, func() bool {
		var oh struct {
			Jobs []struct {
				Stored *struct {
					Bytes int64 `json:"bytes"`
				} `json:"stored"`
			} `json:"jobs"`
		}
		must(t, json.Unmarshal(e.offsite("agent", "show", fqdn), &oh))
		return len(oh.Jobs) == 1 && oh.Jobs[0].Stored != nil && oh.Jobs[0].Stored.Bytes > 0
	})
	var offEvents []event
	must(t, json.Unmarshal(e.offsite("agent", "events", fqdn), &offEvents))
	if countStatus(offEvents, "data", "success") < 2 {
		t.Fatalf("each copy must be a run in the off-site history: %+v", offEvents)
	}
	var oh map[string]any
	must(t, json.Unmarshal(e.offsite("agent", "show", fqdn), &oh))
	j := oh["jobs"].([]any)[0].(map[string]any)
	if j["retention"].(map[string]any)["keep_last"] != float64(1) || j["primary_retention"].(map[string]any)["keep_last"] != float64(1) {
		t.Fatalf("the off-site retention starts as primary's: %+v", j)
	}
	waitFor(t, "the host's page to show the copies", time.Minute, func() bool {
		o := e.offsiteOf(fqdn)
		return o != nil && o.Copies == 2 && o.Pending == 0
	})

	// A host may not read its off-site copy unless a restore from off-site lets it.
	resp, err := e.agentClient(agent).Get(e.url + "/gateway/v1/offsite/" + fqdn + "/config")
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the host read its off-site copy with no restore: %d", resp.StatusCode)
	}

	// The hold: the off-site storage fails. Two backups, neither copied; the daily run forgets the older one only once
	// it is copied.
	mc(t, "admin", "user", "disable", "hf", e.offKey)
	s3 := backup()
	s4 := backup()
	waitFor(t, "the host's page to show what is not copied", time.Minute, func() bool {
		o := e.offsiteOf(fqdn)
		return o != nil && o.Pending == 2
	})
	var pend struct {
		Pending []struct{ ID, Reason string }
	}
	must(t, json.Unmarshal(e.cli("pending", fqdn), &pend))
	if !slices.ContainsFunc(pend.Pending, func(p struct{ ID, Reason string }) bool { return p.ID == s3 && p.Reason == "offsite_copy" }) {
		t.Fatalf("the snapshot not copied yet must wait: %+v", pend)
	}
	e.cli("retention", "run", fqdn)
	waitFor(t, "the daily run to forget what was copied", 2*time.Minute, func() bool {
		return slices.Equal(presentIDs(e.snaps(fqdn, "--refresh")), sortedIDs(s3, s4))
	})
	mc(t, "admin", "user", "enable", "hf", e.offKey)
	waitFor(t, "the copies to catch up", 3*time.Minute, func() bool {
		rows := e.snaps(fqdn, "--offsite")
		_, ok3 := copyOf(rows, s3)
		_, ok4 := copyOf(rows, s4)
		return ok3 && ok4
	})
	waitFor(t, "nothing left to copy", time.Minute, func() bool { o := e.offsiteOf(fqdn); return o != nil && o.Pending == 0 })
	e.cli("retention", "run", fqdn)
	waitFor(t, "the held snapshot to go", 2*time.Minute, func() bool { return slices.Equal(presentIDs(e.snaps(fqdn, "--refresh")), []string{s4}) })
	if got := presentIDs(e.snaps(fqdn, "--offsite", "--refresh")); len(got) != 4 {
		t.Fatalf("off-site keeps its copies under its own retention, which has not run: %v", got)
	}

	// Deleted in primary: an admin deletes s4; the off-site copy shows it, and stays until an admin deletes it there too.
	e.cli("snapshot", "delete", fqdn, s4[:12], "--now", "--confirm", "web01", "-m", "e2e")
	var c4 snapRow
	waitFor(t, "the copy marked deleted in primary", 2*time.Minute, func() bool {
		var ok bool
		c4, ok = copyOf(e.snaps(fqdn, "--offsite"), s4)
		return ok && c4.DeletedInPrimary != nil
	})
	e.offsite("snapshot", "delete", fqdn, c4.ID[:12], "--now", "--confirm", "web01", "-m", "deleted in primary")
	waitFor(t, "the copy deleted off-site too", 2*time.Minute, func() bool { _, ok := copyOf(e.snaps(fqdn, "--offsite", "--refresh"), s4); return !ok })

	// A copy deleted off-site is not made again while primary still has the snapshot.
	s5 := backup()
	var c5 snapRow
	waitFor(t, "the copy of s5", 2*time.Minute, func() bool { var ok bool; c5, ok = copyOf(e.snaps(fqdn, "--offsite"), s5); return ok })
	e.offsite("snapshot", "delete", fqdn, c5.ID[:12], "--now", "--confirm", "web01", "-m", "not wanted off-site")
	waitFor(t, "the copy of s5 deleted", 2*time.Minute, func() bool { _, ok := copyOf(e.snaps(fqdn, "--offsite", "--refresh"), s5); return !ok })
	time.Sleep(8 * time.Second) // a few polls
	if _, ok := copyOf(e.snaps(fqdn, "--offsite", "--refresh"), s5); ok {
		t.Fatal("a snapshot whose copy was deleted off-site was copied again")
	}

	// Exclude: no copies until Include.
	e.cli("offsite", "exclude", fqdn, "data", "-m", "e2e")
	waitFor(t, "the host's page to show the exclusion", time.Minute, func() bool {
		o := e.offsiteOf(fqdn)
		return o != nil && len(o.Jobs) == 1 && o.Jobs[0].Excluded
	})
	s6 := backup()
	time.Sleep(8 * time.Second)
	if _, ok := copyOf(e.snaps(fqdn, "--offsite", "--refresh"), s6); ok {
		t.Fatal("an excluded job was copied")
	}
	e.cli("offsite", "include", fqdn, "data")
	var c6 snapRow
	waitFor(t, "the copy of s6 after Include", 2*time.Minute, func() bool { var ok bool; c6, ok = copyOf(e.snaps(fqdn, "--offsite"), s6); return ok })

	// A restore on the host from its off-site copy, once the server's view lists it.
	waitFor(t, "the copy of s6 listed for a restore", time.Minute, func() bool {
		var copies []struct{ ID, Original string }
		_ = json.Unmarshal(e.cli("offsite", "copies", fqdn), &copies)
		return slices.ContainsFunc(copies, func(c struct{ ID, Original string }) bool { return c.ID == c6.ID && c.Original == s6 })
	})
	ev, err := e.restoreWait(fqdn, c6.ID[:12], "--from-offsite", "-m", "e2e: from off-site")
	if err != nil || ev.Status != "success" || ev.Detail["from_offsite"] != "true" {
		t.Fatalf("restore from off-site: %+v %v", ev, err)
	}
	got, err := os.ReadFile(filepath.Join(ev.Detail["directory"], src, fmt.Sprintf("f%d.txt", backups)))
	if err != nil || string(got) != fmt.Sprintf("version %d\n", backups) {
		t.Fatalf("the restored file: %q %v", got, err)
	}
	var offAudit []auditEntry
	must(t, json.Unmarshal(e.offsite("audit", "--host", fqdn), &offAudit))
	if !slices.ContainsFunc(offAudit, func(x auditEntry) bool { return x.Action == "restore.granted" }) {
		t.Fatalf("the off-site audit log must record the grant: %+v", offAudit)
	}
	// The restore is reported: the host may not read its copy any more.
	resp, err = e.agentClient(agent).Get(e.url + "/gateway/v1/offsite/" + fqdn + "/config")
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the host still reads its off-site copy after the restore: %d", resp.StatusCode)
	}

	m := metrics(t, e.url+"/metrics")
	if _, ok := m[`holdfast_offsite_behind_seconds{host="web01.example.com",job_id="data"}`]; !ok {
		t.Fatalf("the lag metric: %v", m)
	}
	if m[`holdfast_offsite_copy_failed{host="web01.example.com"}`] != 0 {
		t.Fatal("the copy is reported failing")
	}

	// Decision 053: the off-site retention follows primary's until one is saved off-site, which is the job's own and
	// stays; Match primary makes it follow again. Every step keeps more, so no cut waits 7 days.
	offRetention := func() (float64, bool) {
		var oh struct {
			Jobs []struct {
				Retention struct {
					KeepLast float64 `json:"keep_last"`
				}
				Own bool `json:"own_retention"`
			}
		}
		must(t, json.Unmarshal(e.offsite("agent", "show", fqdn), &oh))
		return oh.Jobs[0].Retention.KeepLast, oh.Jobs[0].Own
	}
	e.cli("job", "set", fqdn, "data", "--keep-last", "2", testAccept)
	waitFor(t, "the off-site retention to follow primary's", time.Minute, func() bool { n, own := offRetention(); return n == 2 && !own })
	e.offsite("job", "set", fqdn, "data", "--keep-last", "5", testAccept)
	if n, own := offRetention(); n != 5 || !own {
		t.Fatalf("a retention saved off-site is the job's own: keep_last %v, own %v", n, own)
	}
	e.cli("job", "set", fqdn, "data", "--keep-last", "6", testAccept)
	time.Sleep(8 * time.Second) // a few polls of the off-site instance
	if n, own := offRetention(); n != 5 || !own {
		t.Fatalf("the job's own off-site retention must stay when primary's changes: keep_last %v, own %v", n, own)
	}
	e.cli("offsite", "match-primary", fqdn, "data", testAccept)
	waitFor(t, "Match primary to make it primary's again", time.Minute, func() bool { n, own := offRetention(); return n == 6 && !own })
	e.cli("job", "set", fqdn, "data", "--keep-last", "7", testAccept)
	waitFor(t, "the off-site retention to follow primary's again", time.Minute, func() bool { n, own := offRetention(); return n == 7 && !own })

	// The server's own pages show the off-site copies (decision 053): the host page's Off-site column, and the job
	// page's Off-site view, with the same squares, whether the retention is primary's, and its settings.
	ctx := newBrowser(t)
	errs := jsErrors(ctx)
	run(t, ctx, "root's link into the UI", chromedp.Navigate(e.rootLink()), chromedp.WaitVisible(`nav.side`, chromedp.ByQuery))
	// The Overview's site B tile counts the agents whose copies fail or lag; nothing says it is not set up.
	open(t, ctx, "#/")
	waitText(t, ctx, "copies failing or behind")
	var notSet bool
	run(t, ctx, "the Overview's tiles", chromedp.Evaluate(`document.body.textContent.includes('not set up')`, &notSet))
	if notSet {
		t.Fatal("with an off-site bucket, nothing may say site B is not set up")
	}
	open(t, ctx, "#/agents/"+fqdn)
	waitText(t, ctx, "Last 7 backups")
	waitText(t, ctx, "Off-site")
	// With two sites, the repository box has a row for each, under the sites' labels.
	waitText(t, ctx, "Escrow")
	var repo []string
	run(t, ctx, "the repository box", chromedp.Evaluate(`[...document.querySelectorAll('dl.facts.box dt')].map((d) => d.textContent)`, &repo))
	if strings.Join(repo, ",") != "SITE A,SITE B,Escrow" {
		t.Fatalf("the repository box with off-site copies: %q", repo)
	}
	// The job page's one calendar has both sites, and the site B line its settings.
	open(t, ctx, "#/agents/"+fqdn+"/jobs/data")
	waitText(t, ctx, "Restore points")
	waitText(t, ctx, "the same retention as site A")
	waitText(t, ctx, "Stop copying")
	// A snapshot with an off-site copy names it, and its Restore tab asks where from.
	open(t, ctx, "#/agents/"+fqdn+"/snapshots/"+s6+"/restore")
	waitText(t, ctx, "copy "+c6.ID[:8])
	waitText(t, ctx, "Restore from")
	clickText(t, ctx, "button", "Site B copy")
	waitText(t, ctx, "from site B")
	if errs := errs(); len(errs) > 0 {
		t.Fatalf("the UI logged errors:\n%s", strings.Join(errs, "\n"))
	}
}

// Without offsite: Holdfast is complete with one bucket, and shows nothing about off-site (decision 041).
func TestNoOffsite(t *testing.T) {
	e := newEnv(t)
	const fqdn = "web01.example.com"
	e.startAgent(fqdn, e.addHost(fqdn, "data", "--path", smallSource(t), "--daily", "02:00"))
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	if _, ok := e.hostRaw(fqdn)["offsite"]; ok {
		t.Fatal("the host's page shows off-site copies")
	}
	for _, args := range [][]string{{"offsite", "copies", fqdn}, {"--offsite", "agent", "list"}} {
		if out, err := e.cliErr(args...); err == nil || !strings.Contains(string(out), "not set up") {
			t.Fatalf("holdfast %s without off-site: %v %s", strings.Join(args, " "), err, out)
		}
	}
	var set struct{ Offsite bool }
	must(t, json.Unmarshal(e.cli("settings"), &set))
	if set.Offsite {
		t.Fatal("the settings say off-site is set up")
	}
	for k := range metrics(t, e.url+"/metrics") {
		if strings.HasPrefix(k, "holdfast_offsite_") {
			t.Fatalf("an off-site metric without off-site: %s", k)
		}
	}
}
