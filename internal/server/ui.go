package server

import (
	"net/http"
	"strings"

	"github.com/kabsdk/holdfast/internal/webui"
)

// UIHandler serves the UI listener (decision 034): the login endpoints, root's one-time link,
// /api/v1 behind a session, and the UI's files.
func (s *Server) UIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET "+linkPath, s.handleLink)
	mux.HandleFunc("GET "+callbackPath, s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("POST /auth/device", s.handleDeviceStart)
	mux.HandleFunc("POST /auth/device/token", s.handleDeviceToken)
	mux.Handle("/api/v1/", s.withSession(s.APIHandler(false)))
	files := http.FileServerFS(webui.Files())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(w, r)
	})
	return securityHeaders(mux)
}

// securityHeaders are on every answer of the UI listener: no framing, no sniffing, no referrer, scripts and
// styles only from this origin, and no caching of the API's answers.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
