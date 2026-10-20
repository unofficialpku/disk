// Package gui hosts the authenticated local browser interface.
package gui

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

//go:embed all:web
var assets embed.FS

type server struct {
	application                    *Application
	host, origin, version, control string
	tray                           bool
	tokens                         *tokens
	activate                       func() error
	stop                           func()
	devProxy                       http.Handler
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	// An exact authority check also rejects DNS rebinding and other localhost
	// ports. Forwarded headers are deliberately not used by this loopback server.
	if r.Host != s.host || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != s.origin) {
		http.Error(w, "untrusted origin", http.StatusForbidden)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	// Vite authenticates HMR with its own short token. A GUI ticket/session or
	// control credential is always 43 characters and must never reach Nuxt.
	hmrTokens := r.URL.Query()["token"]
	hmr := s.devProxy != nil && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		r.Header.Get("Sec-WebSocket-Protocol") == "vite-hmr" && strings.HasPrefix(r.URL.Path, "/_nuxt/") &&
		len(hmrTokens) == 1 && hmrTokens[0] != "" && len(hmrTokens[0]) != 43
	if r.URL.Query().Has("ticket") || (!hmr && r.URL.Query().Has("token")) || r.URL.Query().Has("access_token") {
		http.Error(w, "credentials must not be passed in query parameters", http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/internal/activate" || r.URL.Path == "/internal/stop" {
		s.controlRequest(w, r)
		return
	}
	if r.URL.Path == "/internal" || strings.HasPrefix(r.URL.Path, "/internal/") || r.URL.Path == "/api" {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/download" || strings.HasPrefix(r.URL.Path, "/download/") {
		s.browserDownload(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api(w, r)
		return
	}
	s.serveAssets(w, r)
}

func method(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, allowed := range methods {
		if r.Method == allowed {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func bearer(r *http.Request) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(value) != 43 {
		return ""
	}
	return value
}

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/session" {
		if !method(w, r, http.MethodPost) {
			return
		}
		if r.Header.Get("Origin") != s.origin {
			http.Error(w, "same-origin request required", http.StatusForbidden)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var input struct {
			Ticket string `json:"ticket"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || len(input.Ticket) != 43 || !errors.Is(decoder.Decode(new(any)), io.EOF) {
			http.Error(w, "invalid login request", http.StatusBadRequest)
			return
		}
		token, err := s.tokens.exchange(input.Ticket)
		if err != nil {
			http.Error(w, "login link expired or already used; open the interface again", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"token": token, "expires_in": int(sessionLifetime.Seconds())})
		return
	}
	if !s.tokens.authenticated(bearer(r)) {
		http.Error(w, "open the interface from the tray or run pkudisk gui", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != s.origin {
		http.Error(w, "same-origin request required", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/api/status":
		if method(w, r, http.MethodGet) {
			writeJSON(w, map[string]any{"version": s.version, "tray": s.tray, "flags": hostFlags()})
		}
	case "/api/quit":
		if !method(w, r, http.MethodPost) {
			return
		}
		if r.Header.Get("Origin") != s.origin {
			http.Error(w, "same-origin request required", http.StatusForbidden)
			return
		}
		if s.application != nil {
			if err := s.application.BeginShutdown(); err != nil {
				writeError(w, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		s.stop()
	default:
		if s.application != nil {
			s.application.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	}
}

func (s *server) controlRequest(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	// Control credentials are separate from browser sessions and are never
	// returned to the page. Browser requests are not part of this protocol.
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" ||
		subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.control)) != 1 {
		http.Error(w, "unauthorized control request", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/internal/stop" {
		if s.application != nil {
			if err := s.application.BeginShutdown(); err != nil {
				writeError(w, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		s.stop()
		return
	}
	if err := s.activate(); err != nil {
		http.Error(w, "could not open the default browser", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
