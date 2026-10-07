// Package api serves the ferrous agent's HTTP API: bearer auth, request
// logging, JSON errors, discovery, power, stats and log streaming.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"ferrous/agent/internal/docker"
	"ferrous/agent/internal/rcon"
)

// Server holds handler dependencies.
type Server struct {
	docker  *docker.Client
	token   string
	version string
	log     *slog.Logger
	started time.Time

	hubsMu sync.Mutex           // guards hubs
	hubs   map[string]*rcon.Hub // container id → persistent rcon connection
}

func New(d *docker.Client, token, version string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{
		docker: d, token: token, version: version, log: log,
		started: time.Now().UTC(),
		hubs:    make(map[string]*rcon.Hub),
	}
}

// Handler builds the full middleware chain:
// logging → routing → (per-route) bearer auth → handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/ping", s.auth(http.HandlerFunc(s.handlePing)))
	mux.Handle("GET /api/v1/system", s.auth(http.HandlerFunc(s.handleSystem)))
	mux.Handle("GET /api/v1/servers", s.auth(http.HandlerFunc(s.handleServers)))
	mux.Handle("POST /api/v1/servers", s.auth(http.HandlerFunc(s.handleDeploy)))
	mux.Handle("POST /api/v1/servers/{id}/power", s.auth(http.HandlerFunc(s.handlePower)))
	mux.Handle("GET /api/v1/servers/{id}/stats", s.auth(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /api/v1/servers/{id}/logs", s.auth(http.HandlerFunc(s.handleLogs)))
	mux.Handle("POST /api/v1/servers/{id}/rcon", s.auth(http.HandlerFunc(s.handleRcon)))
	mux.Handle("GET /api/v1/servers/{id}/rcon", s.auth(http.HandlerFunc(s.handleRconStatus)))
	mux.Handle("GET /api/v1/servers/{id}/files", s.auth(http.HandlerFunc(s.handleFiles)))
	mux.Handle("PUT /api/v1/servers/{id}/files", s.auth(http.HandlerFunc(s.handleFiles)))
	// catch-all: unknown paths get the same auth wall, then a JSON 404/405
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

// exactEndpoints lists exact-path endpoints and their allowed methods so the
// catch-all can answer 405 (with Allow) for wrong-method requests instead of
// masking them as 404. ServeMux's own method-mismatch detection is shadowed
// by the catch-all pattern.
var exactEndpoints = map[string][]string{
	"/api/v1/ping":    {http.MethodGet},
	"/api/v1/system":  {http.MethodGet},
	"/api/v1/servers": {http.MethodGet, http.MethodPost},
}

// paramEndpoints maps parameterized path suffixes to their allowed methods
// (the exact-path table can't match ids like /servers/abc/rcon).
var paramEndpoints = []struct {
	suffix  string
	methods []string
}{
	{"/power", []string{http.MethodPost}},
	{"/stats", []string{http.MethodGet}},
	{"/logs", []string{http.MethodGet}},
	{"/rcon", []string{http.MethodGet, http.MethodPost}},
	{"/files", []string{http.MethodGet, http.MethodPut}},
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if ms, ok := exactEndpoints[r.URL.Path]; ok && !slices.Contains(ms, r.Method) {
		w.Header().Set("Allow", strings.Join(ms, ", "))
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/servers/") {
		for _, pe := range paramEndpoints {
			if strings.HasSuffix(r.URL.Path, pe.suffix) && !slices.Contains(pe.methods, r.Method) {
				w.Header().Set("Allow", strings.Join(pe.methods, ", "))
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
				return
			}
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// writeDockerError maps engine failures to our error contract: unknown
// container → 404, anything else (daemon down, API error) → 502 JSON — a
// friendly host-level error the panel can render, never a crash.
func writeDockerError(w http.ResponseWriter, err error) {
	var ae *docker.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such container"})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}
