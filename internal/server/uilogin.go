package server

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Root's way into the web UI: `holdfast ui-login`, as root on the server, asks the root socket for a
// one-time link, valid 5 minutes, that opens a browser session as local:root, admin on every host, for 12 hours. It
// is the UI without an identity provider, and the browser's break-glass login when the provider is down. The link is
// a bearer secret shown in a terminal; it is worth no more than root on the server, which it needs.

const (
	LinkLife = 5 * time.Minute

	// rootSubject is the subject of a session opened with root's link. An OIDC login never has it, so only a
	// link makes such a session admin everywhere.
	rootSubject  = "holdfast:root-link"
	rootUsername = "local:root"
	linkPath     = "/auth/link"
)

type loginLinks struct {
	mu    sync.Mutex
	links map[string]loginLink // by the token's sha256
}

type loginLink struct {
	expires  time.Time
	sudoUser string
}

// UILoginResult is what ui-login answers: the link and when it ends.
type UILoginResult struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// apiUILogin is POST /api/v1/ui-login, on the root socket only.
func (s *Server) apiUILogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SudoUser string `json:"sudo_user,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	if s.set.UI.Listen == "" || s.set.UI.PublicURL == "" {
		writeErr(w, http.StatusConflict, "the web UI is off: set ui.listen and ui.public_url in server.yaml (the installer does)")
		return
	}
	now := s.now()
	token := creds.NewToken()
	l := &s.links
	l.mu.Lock()
	if l.links == nil {
		l.links = map[string]loginLink{}
	}
	for k, v := range l.links {
		if now.After(v.expires) {
			delete(l.links, k)
		}
	}
	l.links[creds.HashToken(token)] = loginLink{expires: now.Add(LinkLife), sudoUser: in.SudoUser}
	l.mu.Unlock()
	a := actorOf(r)
	if err := s.st.Q().Audit(r.Context(), store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "auth.link_issued",
		Detail: map[string]any{"sudo_user": in.SudoUser, "expires_at": now.Add(LinkLife)}}); err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, UILoginResult{URL: strings.TrimSuffix(s.set.UI.PublicURL, "/") + linkPath + "?t=" + token, ExpiresAt: now.Add(LinkLife)})
}

// handleLink is GET /auth/link on the UI listener: it uses the link once and opens a browser session as local:root.
func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	hash := creds.HashToken(r.URL.Query().Get("t"))
	l := &s.links
	l.mu.Lock()
	link, ok := l.links[hash]
	delete(l.links, hash)
	l.mu.Unlock()
	if !ok || s.now().After(link.expires) {
		http.Error(w, "this link has expired or was already used: run holdfast ui-login on the server again", http.StatusForbidden)
		return
	}
	token, _, err := s.startSession(r.Context(), identity{Subject: rootSubject, Username: rootUsername}, "browser")
	if err != nil {
		http.Error(w, "the login failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.st.Q().Audit(r.Context(), store.AuditEntry{Actor: rootUsername, Source: "ui", Action: "auth.link_used", Detail: map[string]any{"sudo_user": link.sudoUser}}); err != nil {
		s.log.Warn("auditing the use of a ui-login link", "err", err)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: strings.HasPrefix(s.set.UI.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: int(BrowserSessionLife.Seconds())})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/"><title>Holdfast</title><p><a href="/">Continue</a></p>`)
}

// noProviderPage is the login page of a server without an identity provider.
func (s *Server) noProviderPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// The UI's own stylesheet: its Content-Security-Policy allows no inline style.
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Holdfast: log in</title>
<link rel="stylesheet" href="/style.css">
<body><div class="loginpage panel">
<h1>Log in to Holdfast</h1>
<p>This server has no identity provider set up, so the way in is a link from root. As root on %s, run:</p>
<pre>holdfast ui-login</pre>
<p>It prints a link that works once, within 5 minutes, and logs you in as root, an admin on every host, for 12 hours.</p>
<p class="muted">To let people log in as themselves, with roles from their groups, set up OIDC (<code>auth.oidc</code> in
server.yaml).</p></div>`, html.EscapeString(s.set.NodeName))
}

// oidcOn says whether server.yaml names an identity provider.
func (s *Server) oidcOn() bool { return s.set.Auth.OIDC.Issuer != "" }
