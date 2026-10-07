package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ferrous/agent/internal/docker"
)

const testToken = "unit-test-token"

func newTestServer() *Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(docker.New("/nonexistent/docker.sock"), testToken, "test", log)
}

func do(t *testing.T, s *Server, method, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAuthMissing(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodGet, "/api/v1/ping", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "unauthorized" {
		t.Fatalf("expected JSON unauthorized error, got %q", rec.Body.String())
	}
}

func TestAuthWrongToken(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodGet, "/api/v1/ping", "Bearer nope")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestAuthOKPing(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodGet, "/api/v1/ping", "Bearer "+testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["ok"] != true || body["version"] != "test" {
		t.Fatalf("unexpected ping body: %v", body)
	}
}

func TestNotFoundIsJSON(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodGet, "/api/v1/nope", "Bearer "+testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "not found" {
		t.Fatalf("expected JSON not-found, got %q", rec.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodPost, "/api/v1/ping", "Bearer "+testToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

func TestSystemDockerDownIs502(t *testing.T) {
	// socket path does not exist: docker layer must degrade to a clean 502 JSON,
	// never a panic or 500.
	rec := do(t, newTestServer(), http.MethodGet, "/api/v1/system", "Bearer "+testToken)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("expected JSON error body, got %q", rec.Body.String())
	}
}

func doBody(t *testing.T, s *Server, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPowerValidation(t *testing.T) {
	s := newTestServer()
	a := "Bearer " + testToken

	if rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/power", a, `{"action":"explode"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad action: want 400, got %d", rec.Code)
	}
	if rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/power", a, `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body: want 400, got %d", rec.Code)
	}
	if rec := doBody(t, s, http.MethodGet, "/api/v1/servers/abc/power", a, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: want 405, got %d", rec.Code)
	}
	// dead socket: validation passes, inspect fails → clean 502, never a panic
	if rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/power", a, `{"action":"stop"}`); rec.Code != http.StatusBadGateway {
		t.Errorf("docker down: want 502, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPowerInvalidID(t *testing.T) {
	rec := doBody(t, newTestServer(), http.MethodPost, "/api/v1/servers/ab%20cd/power",
		"Bearer "+testToken, `{"action":"start"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for id with space, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestStatsAndLogsDockerDown(t *testing.T) {
	s := newTestServer()
	a := "Bearer " + testToken
	if rec := do(t, s, http.MethodGet, "/api/v1/servers/abc/stats", a); rec.Code != http.StatusBadGateway {
		t.Errorf("stats: want 502, got %d", rec.Code)
	}
	// logs must fail as JSON *before* SSE headers when docker is unreachable
	if rec := do(t, s, http.MethodGet, "/api/v1/servers/abc/logs", a); rec.Code != http.StatusBadGateway {
		t.Errorf("logs: want 502, got %d", rec.Code)
	}
	if ct := do(t, s, http.MethodGet, "/api/v1/servers/abc/logs", a).Header().Get("Content-Type"); ct == "text/event-stream; charset=utf-8" {
		t.Errorf("logs must not switch to SSE on docker failure")
	}
}

func TestLogsBadTail(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodGet, "/api/v1/servers/abc/logs?tail=banana", "Bearer "+testToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
