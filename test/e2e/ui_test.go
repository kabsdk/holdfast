package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// xp is an XPath for an element whose text contains s.
func xp(tag, s string) string { return `//` + tag + `[contains(normalize-space(.), "` + s + `")]` }

func clickText(t *testing.T, ctx context.Context, tag, s string) {
	t.Helper()
	run(t, ctx, "click "+tag+" "+s, chromedp.WaitVisible(xp(tag, s), chromedp.BySearch), chromedp.Click(xp(tag, s), chromedp.BySearch))
}

func waitText(t *testing.T, ctx context.Context, s string) {
	t.Helper()
	run(t, ctx, "wait for "+s, chromedp.WaitVisible(xp("*", s), chromedp.BySearch))
}

// field fills the input under a label whose caption is span: it empties it the way a person would, with an
// input event the UI hears, then types.
func field(t *testing.T, ctx context.Context, span, value string) {
	t.Helper()
	sel := `//label[span[contains(normalize-space(.), "` + span + `")]]//*[self::input or self::textarea]`
	clear := `(() => { const el = document.evaluate(` + "`" + sel + "`" + `, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null).singleNodeValue;
		el.value = ''; el.dispatchEvent(new Event('input', {bubbles: true})); return true; })()`
	var ok bool
	run(t, ctx, "fill "+span, chromedp.WaitVisible(sel, chromedp.BySearch), chromedp.Evaluate(clear, &ok), chromedp.SendKeys(sel, value, chromedp.BySearch))
}

func open(t *testing.T, ctx context.Context, hash string) {
	t.Helper()
	run(t, ctx, "open "+hash, chromedp.Navigate(uiURL+"/"+hash), chromedp.WaitVisible(`nav.side`, chromedp.ByQuery))
}

// jsErrors collects the page's uncaught exceptions and console errors: the UI must have none.
func jsErrors(ctx context.Context) func() []string {
	var mu sync.Mutex
	var errs []string
	chromedp.ListenTarget(ctx, func(ev any) {
		mu.Lock()
		defer mu.Unlock()
		switch ev := ev.(type) {
		case *runtime.EventExceptionThrown:
			errs = append(errs, "exception: "+ev.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			if ev.Type == runtime.APITypeError {
				var parts []string
				for _, a := range ev.Args {
					parts = append(parts, string(a.Value)+a.Description)
				}
				msg := strings.Join(parts, " ")
				// A refused request is how the UI learns it may not do something; the page shows it.
				if !strings.Contains(msg, "Failed to load resource") {
					errs = append(errs, "console: "+msg)
				}
			}
		}
	})
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, errs...) }
}

// The UI, page by page, as an admin: every page renders without a JavaScript error, and one
// action per page is carried out for real, against the real server, restic and S3.
func TestUIPages(t *testing.T) {
	src := smallSource(t)
	e := newEnvWith(t, envOpts{OIDC: true})
	const web = "web01.example.com"
	// A short retention, so that every save trips R101 and the UI's accept is used below.
	e.startAgent(web, e.addHost(web, "system", "--path", src, "--daily", "02:00", "--keep-daily", "7d", "--keep-last", "3"))
	e.waitProgress(web, "first_backup_done", 2*time.Minute)
	must(t, os.WriteFile(src+"/later.txt", []byte("a second version\n"), 0o644))
	e.cli("job", "run", web, "system")
	waitFor(t, "a second snapshot", 2*time.Minute, func() bool { return len(e.snapshotIDs(web)) >= 2 })
	snaps := e.snapshotIDs(web)

	ctx := newBrowser(t)
	errs := jsErrors(ctx)
	uiLogin(t, ctx, "ada")

	// Overview, Settings, Audit log, Access, Config history, Restores render; the old Problems page opens the Overview.
	for _, p := range []struct{ hash, text string }{
		{"#/", "1 of 1 jobs OK"}, {"#/problems", "All agents"}, {"#/settings", "0.19.1, which the packages ship"}, {"#/audit", "host.add"},
		{"#/access", "holdfast-admins"}, {"#/history", "Revisions"}, {"#/restores", "Choose a snapshot"},
	} {
		open(t, ctx, p.hash)
		waitText(t, ctx, p.text)
	}

	// Overview: a tile filters the agents by what it counts. Nothing has failed, so Failed leaves none.
	open(t, ctx, "#/")
	waitText(t, ctx, web)
	// Under its name, the address the agent connects from: never a hostname it reports (decision 056).
	var sub string
	run(t, ctx, "the line under the agent's name", chromedp.Text(`.hostrow .sub`, &sub, chromedp.ByQuery))
	if !strings.HasPrefix(sub, "from ") {
		t.Fatalf("under the agent's name: %q, want the address it connects from", sub)
	}
	clickText(t, ctx, "button", "last run failed")
	waitText(t, ctx, "No agent matches.")
	clickText(t, ctx, "button", "Show all agents")
	waitText(t, ctx, web)

	// Agent: a row per job with what it backs up and its last 14 days, and Back up now. The old #/hosts/ address opens it.
	open(t, ctx, "#/hosts/"+web)
	waitText(t, ctx, "Last 14 days")
	open(t, ctx, "#/agents/"+web)
	waitText(t, ctx, "Backs up")
	waitText(t, ctx, "Set in its agent.yaml")
	clickText(t, ctx, "button", "Back up now")
	waitText(t, ctx, "Queued")

	// The days the agent page draws, as the API and the CLI give them: today has both backups, and all went well.
	var hd struct {
		Jobs []struct {
			JobID string `json:"job_id"`
			Days  []struct {
				Date  string
				Runs  int
				Worst string
			}
		}
	}
	must(t, json.Unmarshal(e.cli("agent", "days", web), &hd))
	if len(hd.Jobs) != 1 || hd.Jobs[0].JobID != "system" || len(hd.Jobs[0].Days) != 14 {
		t.Fatalf("host days: %+v", hd)
	}
	if today := hd.Jobs[0].Days[13]; today.Runs < 2 || today.Worst != "success" {
		t.Fatalf("today's backups, as host days gives them: %+v", today)
	}

	// Configure: the checks come from the server as you type, and the save makes a revision.
	open(t, ctx, "#/agents/"+web+"/configure/system")
	waitText(t, ctx, "What you get")
	waitText(t, ctx, "Files where the agent runs")
	waitText(t, ctx, "holdfast job set "+web+" system")
	field(t, ctx, "Exclude", "*.tmp")
	waitText(t, ctx, "excludes[0]") // the diff of the change
	field(t, ctx, "Message", "exclude temporary files, from the UI")
	// The test job keeps 7 days, so R101 fires, and Save waits until it is accepted.
	waitText(t, ctx, "R101")
	clickText(t, ctx, "label", "I accept")
	waitText(t, ctx, "Ready to save")
	clickText(t, ctx, "button", "Save")
	waitText(t, ctx, "Last 14 days") // back on the agent page
	var revs []struct{ Message string }
	must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
	if revs[0].Message != "exclude temporary files, from the UI" {
		t.Fatalf("the UI's save is not the newest revision: %+v", revs[0])
	}

	// Job: one calendar of restore points, a square a day, with restic's rules as chips that count what each keeps;
	// today is chosen, and lists its restore points. The old snapshot list's and calendar's addresses open it too.
	open(t, ctx, "#/hosts/"+web+"/jobs/system/snapshots")
	waitText(t, ctx, "Restore points")
	waitText(t, ctx, "SITE A")
	waitText(t, ctx, "one per day for 7 days") // --keep-daily 7d
	var days, points int
	run(t, ctx, "the calendar's days with restore points", chromedp.Evaluate(`document.querySelectorAll('.mgrid button.cell:not(.c-none)').length`, &days))
	run(t, ctx, "today's restore points", chromedp.Evaluate(`[...document.querySelectorAll('.gt:not(.head)')].filter((r) => r.querySelector('a.mono')).length`, &points))
	if days < 1 || points < 2 {
		t.Fatalf("the calendar shows %d days with restore points and %d restore points today, want at least 1 and 2", days, points)
	}
	open(t, ctx, "#/agents/"+web+"/jobs/system/calendar")
	waitText(t, ctx, "Restore points")

	// Snapshot: Files is one tree with what changed against the snapshot before (later.txt is new), Only changes, and a
	// find in this snapshot; the old Changes address opens it too. Then a restore's dry run through the agent.
	open(t, ctx, "#/agents/"+web+"/snapshots/"+snaps[0]+"/files")
	waitText(t, ctx, "Changes against")
	waitText(t, ctx, "later.txt")
	// The folders' listings and the diff's marks load apart: the mark comes once the diff is read.
	waitFor(t, "later.txt marked new in the tree", 30*time.Second, func() bool {
		var marked int
		_ = chromedp.Run(ctx, chromedp.Evaluate(`[...document.querySelectorAll('.trow')].filter((r) => r.textContent.includes('later.txt') && r.querySelector('.chg.add')).length`, &marked))
		return marked == 1
	})
	clickText(t, ctx, "button", "Only changes")
	waitText(t, ctx, "later.txt")
	run(t, ctx, "find in this snapshot", chromedp.SendKeys(`.treebar input[type=search]`, "later", chromedp.ByQuery))
	clickText(t, ctx, "button", "Find")
	waitText(t, ctx, "1 match")
	open(t, ctx, "#/agents/"+web+"/snapshots/"+snaps[0]+"/changes")
	waitText(t, ctx, "Changes against")
	// Restore: a tree of ticks. Unticking a folder leaves it out; the dry run goes through the agent.
	open(t, ctx, "#/agents/"+web+"/snapshots/"+snaps[0]+"/restore")
	waitText(t, ctx, "Everything in the snapshot")
	run(t, ctx, "untick the snapshot's first path", chromedp.Click(`.tree [role=treeitem] input[type=checkbox]`, chromedp.ByQuery))
	waitText(t, ctx, "Left out")
	waitText(t, ctx, "Everything except 1 item")
	clickText(t, ctx, "button", "Dry run")
	waitText(t, ctx, "nothing was written")

	// The rarer actions are in the snapshot's ⋯ menu, which opens inside the window, even this narrow one, and its item
	// takes the click.
	run(t, ctx, "open the snapshot's menu", chromedp.Click(`button[aria-label="More snapshot actions"]`, chromedp.ByQuery))
	var left float64
	run(t, ctx, "where the menu opens", chromedp.Evaluate(`document.querySelector('.menu-pop').getBoundingClientRect().left`, &left))
	if left < 0 {
		t.Fatalf("the menu opens %v px off the window's left edge", -left)
	}
	clickText(t, ctx, "button", "Copy the full ID")
	// Delete… is on the page, as a dialog that always asks why (decision 055): Delete in 7 days stays off until a reason
	// is given. Keep it closes it and deletes nothing.
	clickText(t, ctx, "button", "Delete…")
	waitText(t, ctx, "Delete in 7 days")
	deleteOff := func() bool {
		var off bool
		run(t, ctx, "the Delete in 7 days button", chromedp.Evaluate(`[...document.querySelectorAll('.dialog button')].find(b => b.textContent.includes('Delete in 7 days')).disabled`, &off))
		return off
	}
	if !deleteOff() {
		t.Fatal("Delete in 7 days must stay off until a reason is given")
	}
	field(t, ctx, "Why", "a test of the dialog; nothing is deleted")
	if deleteOff() {
		t.Fatal("Delete in 7 days stays off with a reason given")
	}
	clickText(t, ctx, "button", "Keep it")

	// Prune now: the preview, and the button stays off until the short name is typed.
	open(t, ctx, "#/agents/"+web+"/prune-now")
	waitText(t, ctx, "The restic commands")
	var disabled bool
	run(t, ctx, "the Prune button", chromedp.Evaluate(`[...document.querySelectorAll('button')].find(b => b.textContent.includes('snapshots now')).disabled`, &disabled))
	if !disabled {
		t.Fatal("Prune now must stay disabled until the short name is typed")
	}

	// Add agent, with no S3 key (decision 041): the enrollment code and the install command. Its name need not be an
	// FQDN (decision 056).
	const added = "app07"
	open(t, ctx, "#/add-agent")
	field(t, ctx, "Name", added)
	field(t, ctx, "Paths", "/etc")
	clickText(t, ctx, "button", "Add agent and issue enrollment code")
	waitText(t, ctx, "holdfast-agent enroll")
	if out, err := e.cliErr("agent", "show", added); err != nil {
		t.Fatalf("the UI's new agent is not there: %v %s", err, out)
	}

	if errs := errs(); len(errs) > 0 {
		t.Fatalf("the UI logged errors:\n%s", strings.Join(errs, "\n"))
	}
}
