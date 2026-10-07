// Package api serves the ferrous agent's HTTP API: bearer auth, request
// logging, JSON errors, and the phase-1 discovery endpoints.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"ferrous/agent/internal/docker"
)

// Server holds handler dependencies.
type Server struct {
	docker  *docker.Client
	token   string
	version string
	log     *slog.Logger
	started time.Time
}

func New(d *docker.Client, token, version string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{docker: d, token: token, version: version, log: log, started: time.Now().UTC()}
}

// Handler builds the full middleware chain:
// logging → routing → (per-route) bearer auth → handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/ping", s.auth(http.HandlerFunc(s.handlePing)))
	mux.Handle("GET /api/v1/system", s.auth(http.HandlerFunc(s.handleSystem)))
	mux.Handle("GET /api/v1/servers", s.auth(http.HandlerFunc(s.handleServers)))
	// catch-all: unknown paths get the same auth wall, then a JSON 404
	mux.Handle("/", s.auth(http.HandlerFunc(s.handleNotFound)))
	return s.logging(mux)
}

// --- middleware -------------------------------------------------------------

func (s *Server) auth(next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		ok := strings.HasPrefix(got, prefix) &&
			subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(s.token)) == 1
		if !ok {
			s.log.Warn("auth failed", "ip", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the response code for access logging while preserving
// http.Flusher — required so future SSE endpoints stream unbuffered.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(t0).Milliseconds(), "ip", r.RemoteAddr)
	})
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// knownGET lists GET endpoints so the catch-all can answer 405 (with Allow)
// for wrong-method requests instead of masking them as 404. ServeMux's own
// method-mismatch detection is shadowed by the catch-all pattern.
var knownGET = map[string]bool{
	"/api/v1/ping":    true,
	"/api/v1/system":  true,
	"/api/v1/servers": true,
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if knownGET[r.URL.Path] {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}
