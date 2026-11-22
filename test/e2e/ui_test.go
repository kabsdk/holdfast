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

// The UI, page by page, as an admin (plan-m4 §5): every page renders without a JavaScript error, and one
// action per page is carried out for real, against the real server, restic and S3.
func TestUIPages(t *testing.T) {
	src := smallSource(t)
	e := newEnvWith(t, envOpts{OIDC: true})
	const web = "web01.example.com"
	e.startAgent(web, e.addHost(web, "system", "--path", src, "--daily", "02:00"))
	e.waitProgress(web, "first_backup_done", 2*time.Minute)
	must(t, os.WriteFile(src+"/later.txt", []byte("a second version\n"), 0o644))
	e.cli("job", "run", web, "system")
	waitFor(t, "a second snapshot", 2*time.Minute, func() bool { return len(e.snapshotIDs(web)) >= 2 })
	snaps := e.snapshotIDs(web)

	ctx := newBrowser(t)
	errs := jsErrors(ctx)
	uiLogin(t, ctx, "ada")

	// Fleet, Problems, Settings, Audit log, Access, Config history, Restores render.
	for _, p := range []struct{ hash, text string }{
		{"#/", "Fleet"}, {"#/problems", "Problems"}, {"#/settings", "0.19.1, which the packages ship"}, {"#/audit", "host.add"},
		{"#/access", "holdfast-admins"}, {"#/history", "Revisions"}, {"#/restores", "Choose a snapshot"},
	} {
		open(t, ctx, p.hash)
		waitText(t, ctx, p.text)
	}

	// Host: Back up now.
	open(t, ctx, "#/hosts/"+web)
	waitText(t, ctx, "backup jobs")
	clickText(t, ctx, "button", "Back up now")
	waitText(t, ctx, "Queued")

	// Configure: the checks come from the server as you type, and the save makes a revision.
	open(t, ctx, "#/hosts/"+web+"/configure/system")
	waitText(t, ctx, "What you get")
	waitText(t, ctx, "holdfast job set "+web+" system")
	field(t, ctx, "Exclude", "*.tmp")
	waitText(t, ctx, "excludes[0]") // the diff of the change
	field(t, ctx, "Message", "exclude temporary files, from the UI")
	// The test job keeps only the newest 3, so R101 fires, and Save waits until it is accepted.
	waitText(t, ctx, "R101")
	clickText(t, ctx, "label", "I accept")
	waitText(t, ctx, "Ready to save")
	clickText(t, ctx, "button", "Save")
	waitText(t, ctx, "backup jobs") // back on the host page
	var revs []struct{ Message string }
	must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
	if revs[0].Message != "exclude temporary files, from the UI" {
		t.Fatalf("the UI's save is not the newest revision: %+v", revs[0])
	}

	// Job snapshots: restic's reasons for keeping each one.
	open(t, ctx, "#/hosts/"+web+"/jobs/system/snapshots")
	waitText(t, ctx, "kept:")

	// Snapshot: Files, Changes, and a restore's dry run through the agent.
	open(t, ctx, "#/hosts/"+web+"/snapshots/"+snaps[0]+"/files")
	waitText(t, ctx, "Restore only this")
	open(t, ctx, "#/hosts/"+web+"/snapshots/"+snaps[0]+"/changes")
	waitText(t, ctx, "added")
	open(t, ctx, "#/hosts/"+web+"/snapshots/"+snaps[0]+"/restore")
	clickText(t, ctx, "button", "Dry run")
	waitText(t, ctx, "nothing was written")

	// Prune now: the preview, and the button stays off until the short name is typed.
	open(t, ctx, "#/hosts/"+web+"/prune-now")
	waitText(t, ctx, "The restic commands")
	var disabled bool
	run(t, ctx, "the Prune button", chromedp.Evaluate(`[...document.querySelectorAll('button')].find(b => b.textContent.includes('snapshots now')).disabled`, &disabled))
	if !disabled {
		t.Fatal("Prune now must stay disabled until the short name is typed")
	}

	// Add host, with no S3 key (decision 041): the enrollment code and the install command.
	const added = "app07.example.com"
	open(t, ctx, "#/add-host")
	field(t, ctx, "FQDN", added)
	field(t, ctx, "Paths", "/etc")
	clickText(t, ctx, "button", "Add host and issue enrollment code")
	waitText(t, ctx, "holdfast-agent enroll")
	if out, err := e.cliErr("host", "show", added); err != nil {
		t.Fatalf("the UI's new host is not there: %v %s", err, out)
	}

	if errs := errs(); len(errs) > 0 {
		t.Fatalf("the UI logged errors:\n%s", strings.Join(errs, "\n"))
	}
}
