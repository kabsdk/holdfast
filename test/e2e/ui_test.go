package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
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
		{"#/", "1 of 1 jobs OK"}, {"#/problems", "All agents"}, {"#/settings", "0.19.1, which the packages ship"}, {"#/settings", "has no offsite: section"}, {"#/audit", "host.add"},
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
	// Without an off-site bucket, a tile says site B is not set up, and links to how to set it up (decision 057);
	// Settings says it too.
	var siteB string
	run(t, ctx, "the site B tile", chromedp.Evaluate(`(() => { const t = [...document.querySelectorAll('.tile')].find((x) => x.textContent.includes('not set up'));
		return t ? [t.querySelector('.l').textContent, t.querySelector('.n').textContent, t.querySelector('a').getAttribute('href')].join('|') : ''; })()`, &siteB))
	if siteB != "SITE B off-site|not set up|https://kabsdk.github.io/holdfast/#install-offsite" {
		t.Fatalf("the Overview's site B tile without an off-site bucket: %q", siteB)
	}
	clickText(t, ctx, "button", "last run failed")
	waitText(t, ctx, "No agent matches.")
	clickText(t, ctx, "button", "Show all agents")
	waitText(t, ctx, web)

	// Agent: a row per job with what it backs up and its last 7 backups, and Back up now. The old #/hosts/ address opens it.
	open(t, ctx, "#/hosts/"+web)
	waitText(t, ctx, "Last 7 backups")
	open(t, ctx, "#/agents/"+web)
	waitText(t, ctx, "Backs up")
	waitText(t, ctx, "Stored") // what each job takes in the repository
	waitText(t, ctx, "Set in its agent.yaml")
	// One subject per row: the agent, then its software; the repository's box a row each for it and the escrow file,
	// and with one site, no site label anywhere.
	var heads []string
	run(t, ctx, "the agent page's head", chromedp.Evaluate(`[...document.querySelectorAll('dl.facts.head dt')].map((d) => d.textContent)`, &heads))
	if strings.Join(heads, ",") != "Agent,Software" {
		t.Fatalf("the agent page's head: %q, want a row for the agent and one for its software", heads)
	}
	waitText(t, ctx, "Repository")
	var repo []string
	run(t, ctx, "the repository box", chromedp.Evaluate(`[...document.querySelectorAll('dl.facts.box dt')].map((d) => d.textContent)`, &repo))
	if strings.Join(repo, ",") != "Repository,Escrow" {
		t.Fatalf("the repository box: %q", repo)
	}
	// Back up now and Restore…, a button and a link, are the same size.
	var sizes []float64
	run(t, ctx, "the job row's buttons", chromedp.Evaluate(`[...document.querySelectorAll('.gt button.small, .gt a.button.small')].map((b) => b.getBoundingClientRect().height)`, &sizes))
	if len(sizes) != 2 || sizes[0] != sizes[1] {
		t.Fatalf("Back up now and Restore… are %v px high, want the same", sizes)
	}
	// The ⋯ menu's Remove agent… is red text on the menu's white, not a red box.
	run(t, ctx, "open the agent's menu", chromedp.Click(`button[aria-label="More agent actions"]`, chromedp.ByQuery))
	var look []string
	run(t, ctx, "Remove agent… in the menu", chromedp.Evaluate(`(() => { const b = [...document.querySelectorAll('.menu-item')].find((x) => x.textContent.includes('Remove agent'));
		const c = getComputedStyle(b); return [c.backgroundColor, c.color]; })()`, &look))
	if len(look) != 2 || look[0] != "rgba(0, 0, 0, 0)" || look[1] != "rgb(179, 38, 30)" {
		t.Fatalf("Remove agent… in the menu: background and colour %q, want none and red", look)
	}
	// Its own button closes it again, before anything under it is clicked.
	run(t, ctx, "close the menu", chromedp.Click(`button[aria-label="More agent actions"]`, chromedp.ByQuery), chromedp.WaitNotPresent(`.menu-pop`, chromedp.ByQuery))
	clickText(t, ctx, "button", "Back up now")
	waitText(t, ctx, "Queued")
	// A check, in the recent runs, is of the repository and says what it read.
	e.cli("check")
	waitFor(t, "the agent's check", time.Minute, func() bool {
		for _, ev := range e.events(web) {
			if ev.Kind == "check" {
				return true
			}
		}
		return false
	})
	open(t, ctx, "#/agents/"+web)
	waitText(t, ctx, "sound: its structure, and data part ")

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
	// The last 7 backups the schedule called for, as the API and the CLI give them and the agent page draws them: the
	// job's first backup ran at enrollment and the second by hand, on no slot, so its daily slots came before it.
	var hs struct {
		Jobs []struct {
			JobID string `json:"job_id"`
			Slots []struct{ State string }
		}
	}
	must(t, json.Unmarshal(e.cli("agent", "slots", web), &hs))
	if len(hs.Jobs) != 1 || len(hs.Jobs[0].Slots) != 7 || hs.Jobs[0].Slots[6].State != "before" {
		t.Fatalf("agent slots: %+v", hs)
	}
	var squares, links int
	run(t, ctx, "the job's squares", chromedp.Evaluate(`document.querySelectorAll('.strip.slots > *').length`, &squares), chromedp.Evaluate(`document.querySelectorAll('.strip.slots a').length`, &links))
	if squares != 7 || links != 0 {
		t.Fatalf("the agent page draws %d squares, %d of them links; want 7, none before the first backup", squares, links)
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
	waitText(t, ctx, "Last 7 backups") // back on the agent page
	var revs []struct{ Message string }
	must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
	if revs[0].Message != "exclude temporary files, from the UI" {
		t.Fatalf("the UI's save is not the newest revision: %+v", revs[0])
	}

	// Job: one calendar of restore points, a square a day, with restic's rules as chips that count what each keeps;
	// today is chosen, and lists its restore points. The old snapshot list's and calendar's addresses open it too.
	open(t, ctx, "#/hosts/"+web+"/jobs/system/snapshots")
	waitText(t, ctx, "Restore points")
	waitText(t, ctx, "one per day for 7 days") // --keep-daily 7d
	// Its path and the exclusion the UI saved above fold into one line; what its restore points take is in their line,
	// and with one site no site is named: the repository is the agent's.
	waitText(t, ctx, "1 path, 1 excluded")
	var head string
	var tags int
	run(t, ctx, "the restore points' line", chromedp.Text(`#rp-h`, &head, chromedp.ByQuery), chromedp.Evaluate(`document.querySelectorAll('.sitetag').length`, &tags))
	if !strings.Contains(head, "stored") || tags != 0 {
		t.Fatalf("the restore points' line %q, and %d site labels with one site", head, tags)
	}
	var days, points int
	run(t, ctx, "the calendar's days with restore points", chromedp.Evaluate(`document.querySelectorAll('.mgrid button.cell:not(.c-none)').length`, &days))
	run(t, ctx, "today's restore points", chromedp.Evaluate(`[...document.querySelectorAll('.gt:not(.head)')].filter((r) => r.querySelector('a.mono')).length`, &points))
	if days < 1 || points < 2 {
		t.Fatalf("the calendar shows %d days with restore points and %d restore points today, want at least 1 and 2", days, points)
	}
	// A square on the agent page opens the job's page on its day: ?day= chooses it.
	open(t, ctx, "#/agents/"+web+"/jobs/system?day="+time.Now().AddDate(0, 0, -1).Format(time.DateOnly))
	waitText(t, ctx, "Before the oldest restore point.")
	open(t, ctx, "#/agents/"+web+"/jobs/system/calendar")
	waitText(t, ctx, "Restore points")
	// Find a file in all snapshots is at the top of the job page, above the restore points, so the calendar loading
	// below it never moves it; it finds later.txt.
	var findFirst bool
	run(t, ctx, "where Find a file is", chromedp.Evaluate(`(() => { const f = document.querySelector('.findfile'), r = document.querySelector('#rp-h');
		return !!f && !!r && !!(f.compareDocumentPosition(r) & Node.DOCUMENT_POSITION_FOLLOWING); })()`, &findFirst))
	if !findFirst {
		t.Fatalf("Find a file in all snapshots must come before the restore points:\n%s", page(ctx))
	}
	run(t, ctx, "find later.txt in all snapshots", chromedp.SendKeys(`.findfile input`, "later.txt", chromedp.ByQuery), chromedp.Click(`.findfile button`, chromedp.ByQuery))
	foundText := func() string {
		var found string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('.findfile table.found')?.textContent || ''`, &found))
		return found
	}
	waitFor(t, "later.txt found in the job's snapshots", 60*time.Second, func() bool { return strings.Contains(foundText(), "later.txt") })
	// Its options are restic find's: a day after today, or From after To, turns its field red, says why in the
	// server's words, and turns Find off; Ignore case finds LATER.TXT.
	clickText(t, ctx, "button", "Options")
	findOff := func() bool {
		var off bool
		run(t, ctx, "the Find button", chromedp.Evaluate(`document.querySelector('.findfile form button:not(.link)').disabled`, &off))
		return off
	}
	const toField, fromField = `input[aria-label="Modified to, YYYY-MM-DD"]`, `input[aria-label="Modified from, YYYY-MM-DD"]`
	// setField types into a field as a person's typing ends: the whole value, and an input event the UI hears.
	setField := func(sel, v string) {
		t.Helper()
		var ok bool
		run(t, ctx, "type "+v, chromedp.Evaluate(`(() => { const i = document.querySelector('`+sel+`'); i.value = `+strconv.Quote(v)+`;
			i.dispatchEvent(new Event('input', {bubbles: true})); return true; })()`, &ok))
	}
	tomorrow := time.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	run(t, ctx, "a day after today", chromedp.SendKeys(toField, tomorrow, chromedp.ByQuery))
	waitText(t, ctx, tomorrow+" is after today: no file has been modified then yet.")
	if !findOff() {
		t.Fatal("Find must be off while a day is after today")
	}
	if out, err := e.cliErr("snapshot", "find", web, "later.txt", "--newest", tomorrow); err == nil || !strings.Contains(string(out), "is after today") {
		t.Fatalf("the CLI's find with a day after today: %v %s", err, out)
	}
	setField(toField, "2026-01-01")
	setField(fromField, "2026-02-01")
	waitText(t, ctx, "From (2026-02-01) is after To (2026-01-01).")
	if !findOff() {
		t.Fatal("Find must be off while From is after To")
	}
	var cleared bool
	run(t, ctx, "clear the days", chromedp.Evaluate(`(() => { for (const i of document.querySelectorAll('.datefield input')) { i.value = ''; i.dispatchEvent(new Event('input', {bubbles: true})); } return true; })()`, &cleared))
	clickText(t, ctx, "label", "Ignore case")
	setField(`.findfile form input`, "LATER.TXT")
	run(t, ctx, "find LATER.TXT, ignoring case", chromedp.Click(`.findfile form button:not(.link)`, chromedp.ByQuery))
	waitFor(t, "LATER.TXT found, ignoring case", 60*time.Second, func() bool { return strings.Contains(foundText(), "later.txt") })
	var ic []struct{ Matches []struct{ Path string } }
	must(t, json.Unmarshal(e.cli("snapshot", "find", web, "LATER.TXT", "--ignore-case", "--oldest", "2026-01-01"), &ic))
	if len(ic) == 0 || !strings.HasSuffix(ic[0].Matches[0].Path, "later.txt") {
		t.Fatalf("the CLI's find, ignoring case: %+v", ic)
	}
	// Added per backup: what each backup added, as stored, a bar each over the calendar, and Largest first, which is the
	// API's and the CLI's sort=added. Every snapshot has restic's figure, most first; another sort is refused.
	var bySize []struct {
		ShortID string `json:"short_id"`
		Added   *int64 `json:"data_added_packed"`
	}
	must(t, json.Unmarshal(e.cli("snapshot", "list", web, "--job", "system", "--sort", "added"), &bySize))
	for i, s := range bySize {
		if s.Added == nil || (i > 0 && *s.Added > *bySize[i-1].Added) {
			t.Fatalf("--sort added lists what each backup added, most first: %+v", bySize)
		}
	}
	if _, err := e.cliErr("snapshot", "list", web, "--sort", "size"); err == nil {
		t.Fatal("a sort other than time or added must be refused")
	}
	var bars int
	run(t, ctx, "the bars of Added per backup", chromedp.WaitVisible(`.addchart svg`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('.addchart svg path.bar').length`, &bars))
	if bars < 1 || bars > len(bySize) {
		t.Fatalf("Added per backup draws %d bars for %d restore points", bars, len(bySize))
	}
	clickText(t, ctx, "button", "Largest first")
	waitFor(t, "Largest first lists every restore point", 30*time.Second, func() bool {
		var rows int
		_ = chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('.largest .gt:not(.head)').length`, &rows))
		return rows == len(bySize)
	})

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
	// The same search box, above the tree: its matches open in the tree.
	waitText(t, ctx, "Find in this snapshot")
	run(t, ctx, "find in this snapshot", chromedp.SendKeys(`.findfile form input`, "later.txt", chromedp.ByQuery), chromedp.Click(`.findfile form button:not(.link)`, chromedp.ByQuery))
	waitText(t, ctx, "1 match, in the tree below")
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

	// Roll back: what was deleted or overwritten, to bring back; no time to type (decision 058).
	open(t, ctx, "#/agents/"+web+"/rollback")
	waitText(t, ctx, "Rollbacks of this repository")
	waitText(t, ctx, "holdfast agent rollback "+web)
	var timeFields int
	run(t, ctx, "the rollback page's fields", chromedp.Evaluate(`document.querySelectorAll('input[type=datetime-local], input[type=date]').length`, &timeFields))
	if timeFields != 0 {
		t.Fatal("the rollback page asks for a time; it lists what to bring back")
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
