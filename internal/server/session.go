package server

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Sessions (decision 034). A browser gets a cookie and a CLI a token; the API takes either, on the UI
// listener only. The groups are the ones the provider gave at login.

const (
	BrowserSessionLife = 12 * time.Hour
	BrowserSessionIdle = time.Hour
	CLISessionLife     = 7 * 24 * time.Hour

	sessionCookie = "holdfast_session"
	tokenPrefix   = "hfs_" // so a leaked token is easy to recognise
)

// identity is who the provider says logged in.
type identity struct {
	Subject  string
	Username string
	Groups   []string
}

// startSession stores a new session and returns its token. The login is audited, with whether
// auth.bootstrap_admins made it admin everywhere.
func (s *Server) startSession(ctx context.Context, id identity, kind string) (string, store.Session, error) {
	now := s.now()
	token := tokenPrefix + creds.NewToken()
	sess := store.Session{TokenSHA256: creds.HashToken(token), Kind: kind, Subject: id.Subject, Username: id.Username, Groups: id.Groups,
		CreatedAt: now, ExpiresAt: now.Add(CLISessionLife)}
	if kind == "browser" {
		sess.ExpiresAt, sess.Idle = now.Add(BrowserSessionLife), BrowserSessionIdle
	}
	source := map[string]string{"browser": "ui", "cli": "cli"}[kind]
	err := s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.CreateSession(ctx, sess); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: id.Username, Source: source, Action: "auth.login",
			Detail: map[string]any{"subject": id.Subject, "kind": kind, "groups": id.Groups, "bootstrap_admin": s.bootstrapAdmin(id.Username)}})
	})
	return token, sess, err
}

func (s *Server) bootstrapAdmin(username string) bool {
	return slices.Contains(s.set.Auth.BootstrapAdmins, username)
}

// actorOfSession is the actor a session stands for.
func (s *Server) actorOfSession(sess store.Session) Actor {
	source := "cli"
	if sess.Kind == "browser" {
		source = "ui"
	}
	return Actor{Name: sess.Username, Subject: sess.Subject, Source: source, Groups: sess.Groups, Everywhere: s.bootstrapAdmin(sess.Username)}
}

// sessionToken finds a request's session token: a bearer token (the CLI), or the cookie (a browser).
func sessionToken(r *http.Request) (token string, cookie bool) {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(t), false
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value, true
	}
	return "", false
}

// sameOrigin is the check against cross-site requests: a browser request that changes something must come
// from the UI's own origin. The cookie is SameSite=Strict as well.
func (s *Server) sameOrigin(r *http.Request) bool {
	u, err := url.Parse(s.set.UI.PublicURL)
	if err != nil {
		return false
	}
	return r.Header.Get("Origin") == u.Scheme+"://"+u.Host
}

// withSession lets a request through only with a live session, and makes its actor the session's.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, cookie := sessionToken(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "not logged in: log in to the UI, or run holdfast login")
			return
		}
		if cookie && r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "a request from another site was refused")
			return
		}
		sess, err := s.st.Q().UseSession(r.Context(), creds.HashToken(tok), s.now())
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "your session has ended: log in again")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), s.actorOfSession(sess))))
	})
}

// endSession ends the request's own session.
func (s *Server) endSession(ctx context.Context, r *http.Request) {
	tok, _ := sessionToken(r)
	if tok == "" {
		return
	}
	hash := creds.HashToken(tok)
	if sess, err := s.st.Q().UseSession(ctx, hash, s.now()); err == nil {
		_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: sess.Username, Source: s.actorOfSession(sess).Source, Action: "auth.logout"})
	}
	_ = s.st.Q().DeleteSession(ctx, hash)
}

// Sessions lists the sessions that have not ended, for admins everywhere (plan-m4 Q3).
func (s *Server) Sessions(ctx context.Context, a Actor) ([]store.Session, error) {
	if err := s.requireEverywhere(ctx, a, access.ConfigAccess); err != nil {
		return nil, err
	}
	return s.st.Q().ListSessions(ctx, s.now())
}

// EndSessions ends every session of a user at once, such as after removing them from a group.
func (s *Server) EndSessions(ctx context.Context, a Actor, username string) (int, error) {
	if err := s.requireEverywhere(ctx, a, access.ConfigAccess); err != nil {
		return 0, err
	}
	var n int
	err := s.st.InTx(ctx, func(q *store.Q) error {
		var err error
		if n, err = q.EndSessionsOf(ctx, username); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "access.sessions_ended", Detail: map[string]any{"user": username, "sessions": n}})
	})
	return n, err
}

func (s *Server) apiSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.Sessions(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) apiEndSessions(w http.ResponseWriter, r *http.Request) {
	n, err := s.EndSessions(r.Context(), actorOf(r), r.PathValue("user"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": r.PathValue("user"), "ended": n})
}
