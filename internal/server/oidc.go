package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Login through OIDC (decision 034). The server is the provider's only client:
//   - a browser goes through the authorization code flow, with PKCE and a nonce, and gets a session cookie;
//   - the CLI asks the server to start the device flow (RFC 8628), shows the code to the person, and polls
//     the server until the provider says yes; it then gets a session token.
// The API trusts only its own sessions, never tokens from the provider.

const (
	loginTimeout = 10 * time.Minute // a browser login must finish within this
	callbackPath = "/auth/callback"
	// Logins under way are held in memory until they finish or expire. Starting one needs no session, so
	// their number is capped: a device login also has a goroutine polling the provider.
	maxPendingLogins  = 1000
	maxPendingDevices = 100
)

type pendingLogin struct {
	verifier, nonce, next string
	expires               time.Time
}

type pendingDevice struct {
	expires time.Time
	done    bool
	token   string // the session token, once the provider said yes
	session store.Session
	err     error
}

// oidcClient is the server's side of the provider.
type oidcClient struct {
	mu       sync.Mutex
	provider *oidc.Provider
	http     *http.Client
	logins   map[string]pendingLogin
	devices  map[string]*pendingDevice
}

// providerCtx carries the HTTP client for the provider: one that trusts auth.oidc.ca_file, if set.
func (s *Server) providerCtx(ctx context.Context) (context.Context, error) {
	c := &s.oidc
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
		if f := s.set.Auth.OIDC.CAFile; f != "" {
			pem, err := os.ReadFile(f)
			if err != nil {
				return ctx, fmt.Errorf("auth.oidc.ca_file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return ctx, fmt.Errorf("auth.oidc.ca_file %s holds no certificate", f)
			}
			c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)
	return oidc.ClientContext(ctx, c.http), nil
}

// oauth returns the provider and the client's settings. The provider's discovery document is fetched once,
// at the first login, so a provider that is down at start-up does not stop the server.
func (s *Server) oauth(ctx context.Context) (context.Context, *oidc.Provider, *oauth2.Config, error) {
	ctx, err := s.providerCtx(ctx)
	if err != nil {
		return ctx, nil, nil, err
	}
	o := s.set.Auth.OIDC
	c := &s.oidc
	c.mu.Lock()
	p := c.provider
	c.mu.Unlock()
	if p == nil {
		if p, err = oidc.NewProvider(ctx, o.Issuer); err != nil {
			return ctx, nil, nil, fmt.Errorf("the OIDC provider %s: %w", o.Issuer, err)
		}
		c.mu.Lock()
		c.provider = p
		c.mu.Unlock()
	}
	secret, err := os.ReadFile(o.ClientSecretFile)
	if err != nil {
		return ctx, nil, nil, fmt.Errorf("auth.oidc.client_secret_file: %w", err)
	}
	// The client authenticates with client_secret_post everywhere: x/oauth2 sends nothing else to the device
	// authorization endpoint, and one method for both endpoints is one thing to register at the provider.
	ep := p.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInParams
	return ctx, p, &oauth2.Config{ClientID: o.ClientID, ClientSecret: strings.TrimSpace(string(secret)), Endpoint: ep,
		RedirectURL: strings.TrimSuffix(s.set.UI.PublicURL, "/") + callbackPath, Scopes: o.Scopes}, nil
}

// identityOf checks the ID token and reads who logged in, from the ID token's claims and the provider's
// userinfo, since some providers (Authelia 4.39 among them) put the groups only in the latter.
func (s *Server) identityOf(ctx context.Context, p *oidc.Provider, tok *oauth2.Token, nonce string) (identity, error) {
	o := s.set.Auth.OIDC
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return identity{}, errors.New("the provider sent no ID token; is the openid scope allowed for this client?")
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: o.ClientID}).Verify(ctx, raw)
	if err != nil {
		return identity{}, fmt.Errorf("the ID token: %w", err)
	}
	if nonce != "" && idt.Nonce != nonce {
		return identity{}, errors.New("the ID token's nonce does not match this login")
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return identity{}, err
	}
	if ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
		if ui.Subject != idt.Subject {
			return identity{}, errors.New("the provider's userinfo is for another subject")
		}
		extra := map[string]any{}
		if ui.Claims(&extra) == nil {
			for k, v := range extra {
				if _, ok := claims[k]; !ok {
					claims[k] = v
				}
			}
		}
	}
	id := identity{Subject: idt.Subject, Groups: []string{}}
	id.Username, _ = claims[o.UsernameClaim].(string)
	if id.Username == "" {
		return identity{}, fmt.Errorf("the provider sent no %s claim; set auth.oidc.username_claim, or allow the profile scope", o.UsernameClaim)
	}
	switch g := claims[o.GroupsClaim].(type) {
	case []any:
		for _, x := range g {
			if name, ok := x.(string); ok {
				id.Groups = append(id.Groups, name)
			}
		}
	case string:
		id.Groups = append(id.Groups, g)
	}
	return id, nil
}

func (s *Server) loginFailed(ctx context.Context, kind string, err error) {
	s.log.Warn("a login failed", "kind", kind, "err", err)
	_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: "unknown", Source: map[string]string{"browser": "ui", "cli": "cli"}[kind], Action: "auth.login_failed",
		Detail: map[string]any{"kind": kind, "error": err.Error()}})
}

/* ---------------- the browser: authorization code flow ---------------- */

// safeNext keeps a redirect after login on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.oidcOn() {
		s.noProviderPage(w)
		return
	}
	_, _, cfg, err := s.oauth(r.Context())
	if err != nil {
		s.log.Error("login", "err", err)
		http.Error(w, "login is not available: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	state, nonce, verifier := creds.NewToken(), creds.NewToken(), oauth2.GenerateVerifier()
	c := &s.oidc
	c.mu.Lock()
	if c.logins == nil {
		c.logins = map[string]pendingLogin{}
	}
	now := time.Now()
	for k, l := range c.logins {
		if now.After(l.expires) {
			delete(c.logins, k)
		}
	}
	if len(c.logins) >= maxPendingLogins {
		c.mu.Unlock()
		http.Error(w, "too many logins are under way; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	c.logins[state] = pendingLogin{verifier: verifier, nonce: nonce, next: safeNext(r.URL.Query().Get("next")), expires: now.Add(loginTimeout)}
	c.mu.Unlock()
	http.Redirect(w, r, cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	c := &s.oidc
	state := r.URL.Query().Get("state")
	c.mu.Lock()
	l, ok := c.logins[state]
	delete(c.logins, state)
	c.mu.Unlock()
	if !ok || time.Now().After(l.expires) {
		http.Error(w, "this login has expired or was already used; start again", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		s.loginFailed(r.Context(), "browser", fmt.Errorf("the provider said %s: %s", e, r.URL.Query().Get("error_description")))
		http.Error(w, "the provider refused the login: "+e, http.StatusForbidden)
		return
	}
	ctx, p, cfg, err := s.oauth(r.Context())
	if err == nil {
		var tok *oauth2.Token
		if tok, err = cfg.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(l.verifier)); err == nil {
			var id identity
			if id, err = s.identityOf(ctx, p, tok, l.nonce); err == nil {
				var token string
				if token, _, err = s.startSession(r.Context(), id, "browser"); err == nil {
					http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
						Secure: strings.HasPrefix(s.set.UI.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: int(BrowserSessionLife.Seconds())})
					// A page, not a redirect: the provider sent the browser here from another site, so a
					// SameSite=Strict cookie is sent only on a navigation that starts on this one.
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s"><title>Holdfast</title><p><a href="%s">Continue</a></p>`,
						html.EscapeString(l.next), html.EscapeString(l.next))
					return
				}
			}
		}
	}
	s.loginFailed(r.Context(), "browser", err)
	http.Error(w, "the login failed: "+err.Error(), http.StatusForbidden)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if _, cookie := sessionToken(r); cookie && !s.sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "a request from another site was refused")
		return
	}
	s.endSession(r.Context(), r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		Secure: strings.HasPrefix(s.set.UI.PublicURL, "https://"), SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

/* ---------------- the CLI: device flow ---------------- */

// DeviceStart is what the CLI shows the person: where to go, and the code to enter there.
type DeviceStart struct {
	Request                 string    `json:"request"` // the CLI polls with this
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	Interval                int64     `json:"interval"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// DeviceToken is the session the CLI gets once the person said yes.
type DeviceToken struct {
	Token     string    `json:"token"`
	Username  string    `json:"username"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !s.oidcOn() {
		writeErr(w, http.StatusServiceUnavailable, "this server has no identity provider, so holdfast login cannot work: use holdfast as root on the server, "+
			"or holdfast ui-login there for the web UI; or set up OIDC (auth.oidc in server.yaml)")
		return
	}
	ctx, p, cfg, err := s.oauth(context.WithoutCancel(r.Context()))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "login is not available: "+err.Error())
		return
	}
	if cfg.Endpoint.DeviceAuthURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "the OIDC provider does not offer the device flow, which holdfast login needs")
		return
	}
	c := &s.oidc
	c.mu.Lock()
	if c.devices == nil {
		c.devices = map[string]*pendingDevice{}
	}
	for k, x := range c.devices {
		if time.Now().After(x.expires.Add(time.Minute)) {
			delete(c.devices, k)
		}
	}
	busy := len(c.devices) >= maxPendingDevices
	c.mu.Unlock()
	if busy {
		writeErr(w, http.StatusServiceUnavailable, "too many logins are under way; try again in a few minutes")
		return
	}
	da, err := cfg.DeviceAuth(ctx, oauth2.SetAuthURLParam("client_secret", cfg.ClientSecret))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "the OIDC provider refused to start a device login: "+err.Error())
		return
	}
	id := creds.NewToken()
	// The provider says when the code expires; Holdfast waits no longer than its own login timeout.
	d := &pendingDevice{expires: time.Now().Add(loginTimeout)}
	if !da.Expiry.IsZero() && da.Expiry.Before(d.expires) {
		d.expires = da.Expiry
	}
	c.mu.Lock()
	c.devices[id] = d
	c.mu.Unlock()
	// The server polls the provider for the CLI, so the CLI never holds the client secret.
	go func() {
		pctx, cancel := context.WithDeadline(ctx, d.expires)
		defer cancel()
		tok, err := cfg.DeviceAccessToken(pctx, da)
		var token string
		var sess store.Session
		if err == nil {
			var who identity
			if who, err = s.identityOf(pctx, p, tok, ""); err == nil {
				token, sess, err = s.startSession(s.background(), who, "cli")
			}
		}
		if err != nil {
			s.loginFailed(s.background(), "cli", err)
		}
		c.mu.Lock()
		d.done, d.token, d.session, d.err = true, token, sess, err
		c.mu.Unlock()
	}()
	writeJSON(w, http.StatusOK, DeviceStart{Request: id, UserCode: da.UserCode, VerificationURI: da.VerificationURI,
		VerificationURIComplete: da.VerificationURIComplete, Interval: max(da.Interval, 1), ExpiresAt: d.expires})
}

func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Request string `json:"request"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	c := &s.oidc
	c.mu.Lock()
	d, ok := c.devices[in.Request]
	var done bool
	var token string
	var sess store.Session
	var err error
	if ok {
		done, token, sess, err = d.done, d.token, d.session, d.err
		if done {
			delete(c.devices, in.Request)
		}
	}
	c.mu.Unlock()
	switch {
	case !ok:
		writeErr(w, http.StatusNotFound, "no such login, or it was already collected; run holdfast login again")
	case !done:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
	case err != nil:
		writeErr(w, http.StatusForbidden, "the login failed: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, DeviceToken{Token: token, Username: sess.Username, ExpiresAt: sess.ExpiresAt})
	}
}
