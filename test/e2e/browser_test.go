package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// A headless Chrome, driven over the DevTools protocol: the browser tests use the UI as a person
// does, against a real server, a real Authelia, a real restic and a real S3.

// newBrowser returns a fresh browser context: its own cookies, as a private window has.
func newBrowser(t *testing.T) context.Context {
	t.Helper()
	if browserURL == "" {
		t.Skip("needs the e2e browser (HOLDFAST_BROWSER; make e2e starts it)")
	}
	actx, cancel := chromedp.NewRemoteAllocator(context.Background(), browserURL)
	t.Cleanup(cancel)
	ctx, cancel2 := chromedp.NewContext(actx, chromedp.WithNewBrowserContext())
	t.Cleanup(cancel2)
	// The tab belongs to the context of the first Run: open it here, so the steps' own timeouts do not
	// close it.
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("the browser at %s: %v", browserURL, err)
	}
	return ctx
}

// page returns what the browser shows, to explain a failure.
func page(ctx context.Context) string {
	var loc, body string
	_ = chromedp.Run(ctx, chromedp.Location(&loc), chromedp.Text("body", &body, chromedp.ByQuery))
	return loc + "\n" + tail(body, 3000)
}

func run(t *testing.T, ctx context.Context, what string, actions ...chromedp.Action) {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := chromedp.Run(c, actions...); err != nil {
		t.Fatalf("%s: %v\n--- the browser shows:\n%s", what, err, page(ctx))
	}
}

// authelia logs a user in on Authelia's own form, which the browser has been sent to.
func authelia(t *testing.T, ctx context.Context, user string) {
	t.Helper()
	run(t, ctx, "Authelia's login form", chromedp.WaitVisible(`#username-textfield`, chromedp.ByQuery),
		chromedp.SendKeys(`#username-textfield`, user, chromedp.ByQuery),
		chromedp.SendKeys(`#password-textfield`, oidcPassword(t, user), chromedp.ByQuery),
		chromedp.Click(`#sign-in-button`, chromedp.ByQuery))
}

// uiLogin opens the UI, which sends the browser to Authelia, and logs in there.
func uiLogin(t *testing.T, ctx context.Context, user string) {
	t.Helper()
	run(t, ctx, "open the UI", chromedp.Navigate(uiURL+"/"))
	authelia(t, ctx, user)
	run(t, ctx, "back in the UI after the login", chromedp.WaitVisible(`nav.side`, chromedp.ByQuery))
}

// watchFor makes every page the browser opens from now on note, in its origin's localStorage under key, whether it
// ever showed text containing needle (in any case), from before the page's own scripts run. sawText reads the note.
// It catches what a page shows only for a moment, such as an error on its way to another page.
func watchFor(t *testing.T, ctx context.Context, key, needle string) {
	t.Helper()
	js := fmt.Sprintf(`new MutationObserver(() => {
		if (document.body && document.body.innerText.toLowerCase().includes(%q)) localStorage.setItem(%q, '1');
	}).observe(document, { subtree: true, childList: true, characterData: true });`, strings.ToLower(needle), key)
	run(t, ctx, "watch every page for "+needle, chromedp.ActionFunc(func(c context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(js).Do(c)
		return err
	}))
}

// sawText says whether a page of the current origin noted its text under key (watchFor).
func sawText(t *testing.T, ctx context.Context, key string) bool {
	t.Helper()
	var v string
	run(t, ctx, "read the note "+key, chromedp.Evaluate(fmt.Sprintf(`localStorage.getItem(%q) || ''`, key), &v))
	return v != ""
}

// textOf waits for a selector and returns its text.
func textOf(t *testing.T, ctx context.Context, sel string) string {
	t.Helper()
	var s string
	run(t, ctx, "read "+sel, chromedp.WaitVisible(sel, chromedp.ByQuery), chromedp.Text(sel, &s, chromedp.ByQuery))
	return s
}

// cliLogin runs holdfast login for a user, approves the device code in a browser, and returns the
// environment for that user's later CLI commands.
func (e *env) cliLogin(t *testing.T, user string) []string {
	t.Helper()
	cfg := filepath.Join(e.dir, "cli-"+user)
	must(t, os.MkdirAll(cfg, 0o700))
	userEnv := append(os.Environ(), "XDG_CONFIG_HOME="+cfg)
	cmd := exec.Command(filepath.Join(binDir, "holdfast"), "login", "--server", uiURL, "--ca-file", filepath.Join(oidcDir, "ca", "ca.public.crt"))
	cmd.Env = userEnv
	out, err := cmd.StdoutPipe()
	must(t, err)
	cmd.Stderr = cmd.Stdout
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	lines := bufio.NewScanner(out)
	var verify string
	var seen []string
	for lines.Scan() {
		seen = append(seen, lines.Text())
		if m := regexp.MustCompile(`^Open (\S+)`).FindStringSubmatch(lines.Text()); m != nil {
			verify = m[1]
			break
		}
	}
	if verify == "" {
		t.Fatalf("holdfast login printed no URL: %s", strings.Join(seen, "\n"))
	}
	ctx := newBrowser(t)
	run(t, ctx, "open the device login", chromedp.Navigate(verify))
	authelia(t, ctx, user)
	approveDevice(t, ctx)
	done := make(chan error, 1)
	go func() {
		rest, _ := io.ReadAll(out)
		seen = append(seen, string(rest))
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("holdfast login: %v\n%s", err, strings.Join(seen, "\n"))
		}
	case <-time.After(90 * time.Second):
		t.Fatalf("holdfast login did not finish: %s\n--- the browser shows:\n%s", strings.Join(seen, "\n"), page(ctx))
	}
	return userEnv
}

// approveDevice confirms the device code on Authelia's page.
func approveDevice(t *testing.T, ctx context.Context) {
	t.Helper()
	run(t, ctx, "Authelia's device confirmation", chromedp.WaitVisible(`#openid-consent-accept`, chromedp.ByQuery),
		chromedp.Click(`#openid-consent-accept`, chromedp.ByQuery))
}

// cliAs runs the CLI with a user's saved login (no --socket), with --json.
func (e *env) cliAs(userEnv []string, args ...string) ([]byte, error) {
	cmd := exec.Command(filepath.Join(binDir, "holdfast"), append([]string{"--json"}, args...)...)
	cmd.Env = userEnv
	return cmd.CombinedOutput()
}
