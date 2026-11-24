package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// rootLink asks for a one-time link into the UI, as root on the server does with holdfast ui-login.
func (e *env) rootLink() string {
	e.t.Helper()
	var l struct {
		URL string `json:"url"`
	}
	must(e.t, json.Unmarshal(e.cli("ui-login"), &l))
	if l.URL == "" {
		e.t.Fatal("holdfast ui-login gave no link")
	}
	return l.URL
}

// The web UI on a server with no identity provider: the login page says to run holdfast ui-login;
// its link opens the UI as local:root, an admin on every host, once; issuing and using it are audited.
func TestUILoginWithoutProvider(t *testing.T) {
	e := newEnvWith(t, envOpts{UI: true})
	ctx := newBrowser(t)
	errs := jsErrors(ctx)

	run(t, ctx, "open the UI", chromedp.Navigate(uiURL+"/"))
	waitText(t, ctx, "holdfast ui-login")

	link := e.rootLink()
	run(t, ctx, "open the link", chromedp.Navigate(link), chromedp.WaitVisible(`nav.side`, chromedp.ByQuery))
	if top := textOf(t, ctx, "header.top"); !strings.Contains(top, "local:root") || !strings.Contains(top, "admin") || !strings.Contains(top, "via holdfast ui-login") {
		t.Fatalf("the top bar: %q", top)
	}
	open(t, ctx, "#/access") // admins only
	waitText(t, ctx, "Access")

	// The link works once: a second browser gets the refusal, not a session.
	other := newBrowser(t)
	run(t, other, "the link again", chromedp.Navigate(link))
	waitText(t, other, "expired or was already used")

	issued, used := false, false
	for _, a := range e.audit("") {
		issued = issued || a.Action == "auth.link_issued"
		used = used || a.Action == "auth.link_used"
	}
	if !issued || !used {
		t.Fatalf("the audit log: link issued %v, used %v", issued, used)
	}
	if errs := errs(); len(errs) > 0 {
		t.Fatalf("JavaScript errors: %v", errs)
	}
}

// TestScreenshots takes the documentation's pictures of the UI (website/img), against a real server with real agents,
// restic and S3: HOLDFAST_SCREENSHOTS=$PWD/website/img make e2e E2E_ARGS='-run TestScreenshots'. Skipped otherwise.
func TestScreenshots(t *testing.T) {
	dir := os.Getenv("HOLDFAST_SCREENSHOTS")
	if dir == "" {
		t.Skip("HOLDFAST_SCREENSHOTS names the directory for the pictures")
	}
	must(t, os.MkdirAll(dir, 0o755))

	// A small fleet: three hosts backed up, one of them with a job that fails, and one added but not yet enrolled. Its
	// alerts go to a webhook that takes everything, so the Settings page shows them.
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(hook.Close)
	// The hosts back up a small site at /srv/www, as a web server would, so the pictures show real-looking paths.
	src := "/srv/www"
	must(t, os.RemoveAll(src))
	t.Cleanup(func() { os.RemoveAll(src) })
	for name, body := range map[string]string{"index.html": "<h1>Welcome</h1>\n", "about.html": "<h1>About</h1>\n", "robots.txt": "User-agent: *\n",
		"css/site.css": "body { margin: 0 }\n", "img/logo.svg": "<svg/>\n"} {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755))
		must(t, os.WriteFile(filepath.Join(src, name), []byte(body), 0o644))
	}
	e := newEnvWith(t, envOpts{UI: true, Webhook: hook.URL + "/alerts"})
	const web, db, files, app = "web01.example.com", "db01.example.com", "files01.example.com", "app02.example.com"
	e.startAgent(web, e.addHost(web, "system", "--path", src, "--daily", "02:00"))
	e.startAgent(db, e.addHost(db, "system", "--path", src, "--daily", "01:30"))
	filesCode := e.addHost(files, "home", "--path", src, "--daily", "03:00")
	e.cli("job", "set", files, "projects", "--path", "/srv/projects-that-are-gone", testAccept)
	e.startAgent(files, filesCode)
	e.cli("host", "add", app)
	for _, h := range []string{web, db, files} {
		e.waitProgress(h, "first_backup_done", 2*time.Minute)
	}
	must(t, os.WriteFile(filepath.Join(src, "news.html"), []byte("<h1>News</h1>\n"), 0o644))
	e.cli("job", "run", web, "system")
	waitFor(t, "a second snapshot", 2*time.Minute, func() bool { return len(e.snapshotIDs(web)) >= 2 })
	e.cli("job", "run", files, "projects")
	waitFor(t, "files01's failed job", 2*time.Minute, func() bool { return hasProblem(e.host(files), "failed") })
	snap := e.snapshotIDs(web)[0]

	ctx := newBrowser(t)
	shot := func(name string) {
		t.Helper()
		time.Sleep(700 * time.Millisecond) // the polls settle
		var png []byte
		run(t, ctx, "screenshot "+name, chromedp.CaptureScreenshot(&png))
		must(t, os.WriteFile(filepath.Join(dir, name+".png"), png, 0o644))
	}
	run(t, ctx, "the window", chromedp.EmulateViewport(1180, 740, chromedp.EmulateScale(2)))

	run(t, ctx, "open the UI", chromedp.Navigate(uiURL+"/"))
	waitText(t, ctx, "holdfast ui-login")
	shot("login")

	run(t, ctx, "the link", chromedp.Navigate(e.rootLink()), chromedp.WaitVisible(`nav.side`, chromedp.ByQuery))
	open(t, ctx, "#/")
	waitText(t, ctx, app)
	waitText(t, ctx, "the last backup failed") // files01's failed job, under its host
	shot("overview")

	open(t, ctx, "#/hosts/"+web)
	waitText(t, ctx, "Last 14 days")
	shot("host")

	open(t, ctx, "#/hosts/"+web+"/configure/system")
	waitText(t, ctx, "What you get")
	waitText(t, ctx, "holdfast job set "+web+" system")
	shot("configure")

	open(t, ctx, "#/hosts/"+web+"/jobs/system")
	waitText(t, ctx, "What restic keeps")
	shot("job")

	open(t, ctx, "#/hosts/"+web+"/jobs/system/calendar")
	waitText(t, ctx, "with a kept snapshot")
	shot("calendar")

	open(t, ctx, "#/hosts/"+web+"/snapshots/"+snap+"/files?path="+url.QueryEscape(src))
	waitText(t, ctx, "news.html")
	shot("snapshot")

	open(t, ctx, "#/hosts/"+web+"/snapshots/"+snap+"/changes")
	waitText(t, ctx, "news.html")
	shot("changes")

	open(t, ctx, "#/hosts/"+web+"/snapshots/"+snap+"/restore")
	waitText(t, ctx, "Everything in the snapshot")
	shot("restore")

	// A restore done, so the Restores page and the audit log have one to show.
	if ev, err := e.restoreWait(web, snap); err != nil || ev.Status != "success" {
		t.Fatalf("restore: %+v %v", ev, err)
	}
	open(t, ctx, "#/restores")
	waitText(t, ctx, "/var/restore/")
	shot("restores")

	open(t, ctx, "#/history")
	waitText(t, ctx, "Revisions")
	shot("history")

	open(t, ctx, "#/audit")
	waitText(t, ctx, "Audit log")
	shot("audit")

	open(t, ctx, "#/add-host")
	field(t, ctx, "FQDN", "web02.example.com")
	shot("add-host")

	open(t, ctx, "#/settings")
	waitText(t, ctx, "Send a test alert")
	shot("settings")
}
