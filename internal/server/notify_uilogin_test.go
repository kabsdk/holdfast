package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Root's one-time link into the UI: only on the root socket, once, within 5 minutes, and it opens a
// session that is admin everywhere, without an identity provider.
func TestUILoginLink(t *testing.T) {
	s := newTestServer(t)
	ui := httptest.NewServer(s.UIHandler())
	t.Cleanup(ui.Close)
	s.set.UI.Listen, s.set.UI.PublicURL = ":0", ui.URL

	if code, _ := apiAs(t, s.APIHandler(false), root, "POST", "/api/v1/ui-login", map[string]any{}); code != 404 {
		t.Fatalf("ui-login on the UI listener's API: %d, want 404", code)
	}
	code, body := apiAs(t, s.APIHandler(true), root, "POST", "/api/v1/ui-login", map[string]any{"sudo_user": "ada"})
	var res UILoginResult
	if code != 201 || json.Unmarshal(body, &res) != nil || !strings.HasPrefix(res.URL, ui.URL+linkPath+"?t=") {
		t.Fatalf("ui-login: %d %s", code, body)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if resp, err := c.Get(ui.URL + "/api/v1/me"); err != nil || resp.StatusCode != 401 {
		t.Fatalf("before the link: %v %v", resp, err)
	}
	if resp, err := c.Get(res.URL); err != nil || resp.StatusCode != 200 {
		t.Fatalf("the link: %v %v", resp, err)
	}
	resp, err := c.Get(ui.URL + "/api/v1/me")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("after the link: %v %v", resp, err)
	}
	var me struct {
		Name            string `json:"name"`
		AdminEverywhere bool   `json:"admin_everywhere"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &me) != nil || !me.AdminEverywhere {
		t.Fatalf("the link's session must be admin everywhere: %s", b)
	}
	// Once only.
	if resp, err := http.Get(res.URL); err != nil || resp.StatusCode != 403 {
		t.Fatalf("the link a second time: %v %v", resp, err)
	}
	// The login page without a provider says how to get in.
	resp, err = http.Get(ui.URL + "/auth/login")
	b, _ = io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(b), "holdfast ui-login") {
		t.Fatalf("the login page without a provider: %v %s", err, b)
	}
}

// The server sends its own alerts: a problem once when it starts, once when it ends, and the
// heartbeat after each pass.
func TestNotify(t *testing.T) {
	var mu sync.Mutex
	var alerts []Alert
	beats := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/beat" {
			beats++
			return
		}
		var a Alert
		_ = json.NewDecoder(r.Body).Decode(&a)
		alerts = append(alerts, a)
	}))
	t.Cleanup(hook.Close)
	s := newTestServer(t)
	s.set.Escrow.AgeRecipient = "age1fake" // the export problem is not this test's
	s.set.Notify = NotifySettings{WebhookURL: hook.URL + "/hook", HeartbeatURL: hook.URL + "/beat", After: time.Minute}
	recoveryFile := filepath.Join(t.TempDir(), "holdfast-recovery.txt")
	s.set.RecoveryFile = recoveryFile
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(recoveryFile, []byte("x"), 0o600))
	now := time.Now()
	s.now = func() time.Time { return now }
	ctx := t.Context()

	s.notifyDue(ctx) // seen, not yet a minute old: nothing sent
	now = now.Add(2 * time.Minute)
	s.notifyDue(ctx) // sent
	s.notifyDue(ctx) // not again
	must(os.Remove(recoveryFile))
	s.notifyDue(ctx) // ended
	mu.Lock()
	defer mu.Unlock()
	if len(alerts) != 2 || len(alerts[0].Started) != 1 || alerts[0].Started[0].Code != "recovery_file" || len(alerts[1].Ended) != 1 ||
		!strings.Contains(alerts[0].Text, "recovery_file") {
		t.Fatalf("alerts: %+v", alerts)
	}
	if beats != 4 {
		t.Fatalf("heartbeats: %d, want one a pass", beats)
	}
}
