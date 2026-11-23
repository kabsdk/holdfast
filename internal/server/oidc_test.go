package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// fakeProvider is a minimal OIDC provider for the unit tests: the code flow with PKCE, the device flow, and
// userinfo. Like Authelia 4.39 it puts the username and the groups only in userinfo. The e2e tests use a real
// Authelia.
type fakeProvider struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	mu       sync.Mutex
	codes    map[string]fakeCode // code → login
	approved bool                // the person has said yes to the device login
	user     string
	groups   []string
}

type fakeCode struct{ nonce, challenge string }

func newFakeProvider(t *testing.T, user string, groups ...string) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{key: key, codes: map[string]fakeCode{}, user: user, groups: groups}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json") // oauth2 reads anything else as a form
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	iss := p.srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss, "authorization_endpoint": iss + "/authorize", "token_endpoint": iss + "/token",
			"device_authorization_endpoint": iss + "/device", "userinfo_endpoint": iss + "/userinfo", "jwks_uri": iss + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE is required", 400)
			return
		}
		code := "code-" + q.Get("state")[:8]
		p.mu.Lock()
		p.codes[code] = fakeCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dev-1", "user_code": "WDJB-MJHT", "verification_uri": iss + "/activate",
			"verification_uri_complete": iss + "/activate?user_code=WDJB-MJHT", "interval": 1, "expires_in": 60})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		nonce := ""
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			p.mu.Lock()
			c, ok := p.codes[r.Form.Get("code")]
			delete(p.codes, r.Form.Get("code"))
			p.mu.Unlock()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
				t.Logf("fake provider: token request %v; code known %v; challenge %q", r.Form, ok, c.challenge)
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			nonce = c.nonce
		default:
			t.Logf("fake provider: token request %v", r.Form)
		case "urn:ietf:params:oauth:grant-type:device_code":
			p.mu.Lock()
			ok := p.approved
			p.mu.Unlock()
			if !ok {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + p.user, "token_type": "Bearer", "expires_in": 300, "id_token": p.idToken(t, nonce)})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-"+p.user {
			http.Error(w, "no", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "sub-" + p.user, "preferred_username": p.user, "groups": p.groups})
	})
	return p
}

func (p *fakeProvider) idToken(t *testing.T, nonce string) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{"iss": p.srv.URL, "sub": "sub-" + p.user, "aud": "holdfast", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	b, _ := json.Marshal(claims)
	jws, err := signer.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := jws.CompactSerialize()
	return raw
}

// uiServer starts the UI listener of a test server whose provider is p.
func uiServer(t *testing.T, p *fakeProvider) (*Server, *httptest.Server) {
	t.Helper()
	s := newTestServer(t)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ui := httptest.NewServer(s.UIHandler())
	t.Cleanup(ui.Close)
	s.set.UI.PublicURL = ui.URL
	s.set.Auth.OIDC = OIDCSettings{Issuer: p.srv.URL, ClientID: "holdfast", ClientSecretFile: secret, Scopes: []string{"openid", "profile", "groups"},
		GroupsClaim: "groups", UsernameClaim: "preferred_username"}
	return s, ui
}

func TestBrowserLogin(t *testing.T) {
	p := newFakeProvider(t, "ada", "holdfast-admins")
	_, ui := uiServer(t, p)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	if resp, _ := c.Get(ui.URL + "/api/v1/me"); resp.StatusCode != 401 {
		t.Fatalf("without a session: %d", resp.StatusCode)
	}
	// /auth/login → the provider → /auth/callback, which sets the cookie and sends the browser on.
	resp, err := c.Get(ui.URL + "/auth/login?next=/hosts")
	if err != nil || resp.StatusCode != 200 {
		b := new(strings.Builder)
		if resp != nil {
			_, _ = io.Copy(b, resp.Body)
		}
		t.Fatalf("login: %v %v: %s", resp.Status, err, b)
	}
	u, _ := url.Parse(ui.URL)
	cookies := jar.Cookies(u)
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || !strings.HasPrefix(cookies[0].Value, tokenPrefix) {
		t.Fatalf("session cookie: %+v", cookies)
	}
	resp, _ = c.Get(ui.URL + "/api/v1/me")
	var me Me
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if me.Name != "ada" || me.Subject != "sub-ada" || me.Source != "ui" || !me.AdminEverywhere {
		t.Fatalf("me after login: %+v", me)
	}
	// A change with the cookie must come from the UI's own origin.
	req, _ := http.NewRequest("PUT", ui.URL+"/api/v1/access", strings.NewReader(`{"bindings":[]}`))
	if resp, _ := c.Do(req); resp.StatusCode != 403 {
		t.Fatalf("a cross-site change must be refused: %d", resp.StatusCode)
	}
	if h := resp.Header.Get("Content-Security-Policy"); !strings.Contains(h, "frame-ancestors 'none'") {
		t.Fatalf("security headers: %q", h)
	}
	// Logout ends the session.
	req, _ = http.NewRequest("POST", ui.URL+"/auth/logout", nil)
	req.Header.Set("Origin", ui.URL)
	if resp, _ := c.Do(req); resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := c.Get(ui.URL + "/api/v1/me"); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
	// A replayed callback is refused.
	if resp, _ := c.Get(ui.URL + "/auth/callback?code=x&state=nope"); resp.StatusCode != 400 {
		t.Fatalf("an unknown state: %d", resp.StatusCode)
	}
}

func TestDeviceLogin(t *testing.T) {
	p := newFakeProvider(t, "wes", "web-team")
	s, ui := uiServer(t, p)
	post := func(path, body string) (*http.Response, map[string]any) {
		resp, err := http.Post(ui.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp, m
	}
	_, start := post("/auth/device", "")
	if start["user_code"] != "WDJB-MJHT" || start["request"] == "" {
		t.Fatalf("device start: %v", start)
	}
	body := `{"request":"` + start["request"].(string) + `"}`
	if resp, _ := post("/auth/device/token", body); resp.StatusCode != 202 {
		t.Fatalf("before the person says yes: %d", resp.StatusCode)
	}
	p.mu.Lock()
	p.approved = true
	p.mu.Unlock()
	var tok map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, m := post("/auth/device/token", body)
		if resp.StatusCode == 200 {
			tok = m
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if tok == nil || tok["username"] != "wes" {
		_, last := post("/auth/device/token", body)
		t.Fatalf("device token: %v, last answer %v", tok, last)
	}
	req, _ := http.NewRequest("GET", ui.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok["token"].(string))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("me with the CLI token: %v %v", resp, err)
	}
	var me Me
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if me.Name != "wes" || me.Source != "cli" || len(me.Groups) != 1 || me.AdminEverywhere {
		t.Fatalf("me: %+v", me)
	}
	if resp, _ := post("/auth/device/token", body); resp.StatusCode != 404 {
		t.Fatalf("a token is collected once: %d", resp.StatusCode)
	}
	// An admin ends wes's sessions at once.
	if n, err := s.EndSessions(t.Context(), root, "wes"); err != nil || n != 1 {
		t.Fatalf("end sessions: %d %v", n, err)
	}
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("after its sessions were ended: %d", resp.StatusCode)
	}
}
