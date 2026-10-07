package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
