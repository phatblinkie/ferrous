package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"ferrous/agent/internal/docker"
	"ferrous/agent/internal/fakercon"
)

// fakeEngine serves canned Engine API responses over a temp unix socket so
// gate/rcon logic runs end-to-end without a daemon.
func fakeEngine(t *testing.T, inspectJSON, listJSON string) *docker.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			_, _ = w.Write([]byte(listJSON))
		case strings.HasSuffix(r.URL.Path, "/json"):
			if strings.Contains(inspectJSON, "No such container") {
				w.WriteHeader(http.StatusNotFound) // engine contract: 404 + {"message":...}
			}
			_, _ = w.Write([]byte(inspectJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return docker.New(sock)
}

// newFakeEngineServer builds the api server on a fake engine and registers
// hub cleanup BEFORE the fakercon fixture's Close (t.Cleanup is LIFO — hubs
// must stop their connections first or fakercon waits a full read deadline).
func newFakeEngineServer(t *testing.T, inspectJSON, listJSON string) *Server {
	t.Helper()
	s := New(fakeEngine(t, inspectJSON, listJSON), testToken, "test", nil)
	t.Cleanup(s.Close)
	return s
}

// rconInspect builds an inspect payload for a running managed fake-server
// container whose rcon points at 127.0.0.1:<port>.
func rconInspect(port int, extraLabels, extraEnv string) string {
	return fmt.Sprintf(`{
		"Name": "/fake-server",
		"State": {"Status": "running", "Running": true},
		"Config": {
			"Tty": false,
			"Labels": {"ferrous.managed": "true", "ferrous.rcon_port": "%d"%s},
			"Env": ["RCON_PASSWORD=testpw"%s]
		},
		"NetworkSettings": {"IPAddress": "127.0.0.1", "Networks": {}}
	}`, port, extraLabels, extraEnv)
}

func startFakeRcon(t *testing.T) *fakercon.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := fakercon.New(ln, fakercon.Options{Password: "testpw", InvDump: true, Players: 2})
	t.Cleanup(func() { _ = fake.Close() })
	return fake
}

func rconPort(t *testing.T, f *fakercon.Server) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestRconCommandEndToEnd(t *testing.T) {
	fake := startFakeRcon(t)
	s := newFakeEngineServer(t, rconInspect(rconPort(t, fake), "", ""), `{}`)

	// first command must succeed even though the hub is created on-demand
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc123/rcon",
		"Bearer "+testToken, `{"cmd":"status"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		OK  bool   `json:"ok"`
		Out string `json:"out"`
		MS  int64  `json:"ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.OK {
		t.Fatalf("bad body: %v %s", err, rec.Body.String())
	}
	if !strings.Contains(body.Out, "players : 2 (64)") {
		t.Fatalf("unexpected out: %q", body.Out)
	}

	// correlated reply for a JSON command
	rec = doBody(t, s, http.MethodPost, "/api/v1/servers/abc123/rcon",
		"Bearer "+testToken, `{"cmd":"playerlist verbose"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("playerlist: %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fake-1") {
		t.Fatalf("playerlist payload missing players: %s", rec.Body.String())
	}

	// GET diagnostics: connected hub
	rec = do(t, s, http.MethodGet, "/api/v1/servers/abc123/rcon", "Bearer "+testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint: %d", rec.Code)
	}
	var st struct {
		Connected bool `json:"connected"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Connected {
		t.Fatalf("hub should be connected: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "testpw") {
		t.Fatal("password must never appear in status output")
	}
}

func TestRconTimeoutIs504(t *testing.T) {
	fake := startFakeRcon(t)
	s := newFakeEngineServer(t, rconInspect(rconPort(t, fake), "", ""), `{}`)
	// unknown command: Rust stays silent → 504 (panel inventory maps this to 404)
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc123/rcon",
		"Bearer "+testToken, `{"cmd":"definitely.unknown","timeout_ms":600}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "definitely.unknown") {
		t.Fatalf("error should name the command: %s", rec.Body.String())
	}
}

func TestRconValidation(t *testing.T) {
	s := newTestServer() // dead socket: gate never reached for valid bodies
	a := "Bearer " + testToken
	cases := []struct {
		body string
		want int
	}{
		{`{"cmd":""}`, http.StatusBadRequest},
		{`{"cmd":"   "}`, http.StatusBadRequest},
		{`{"cmd":"` + strings.Repeat("x", 501) + `"}`, http.StatusBadRequest},
		{`{"cmd":"status","timeout_ms":100}`, http.StatusBadRequest},
		{`{"cmd":"status","timeout_ms":60000}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
		{`{"cmd":"status"}`, http.StatusBadGateway}, // docker down after validation
	}
	for i, c := range cases {
		rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc123/rcon", a, c.body)
		if rec.Code != c.want {
			t.Errorf("case %d (%.20s): want %d, got %d (%s)", i, c.body, c.want, rec.Code, rec.Body.String())
		}
	}
	// invalid id
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers/bad%20id/rcon", a, `{"cmd":"status"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad id: want 400, got %d", rec.Code)
	}
	// method: DELETE is not allowed on /rcon (GET and POST are)
	rec = doBody(t, s, http.MethodDelete, "/api/v1/servers/abc123/rcon", a, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("delete: want 405, got %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Errorf("Allow must list GET and POST, got %q", allow)
	}
	// auth wall applies
	if rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc123/rcon", "", `{"cmd":"status"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth: want 401, got %d", rec.Code)
	}
}

func TestRconGates(t *testing.T) {
	a := "Bearer " + testToken

	t.Run("unmanaged", func(t *testing.T) {
		inspect := `{"Name":"/x","State":{"Running":true},
			"Config":{"Labels":{"ferrous.rcon_port":"28016"},"Env":["RCON_PASSWORD=p"]},
			"NetworkSettings":{"IPAddress":"127.0.0.1"}}`
		s := newFakeEngineServer(t, inspect, `{}`)
		rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/rcon", a, `{"cmd":"status"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("no rcon label", func(t *testing.T) {
		inspect := `{"Name":"/x","State":{"Running":true},
			"Config":{"Labels":{"ferrous.managed":"true"},"Env":["RCON_PASSWORD=p"]},
			"NetworkSettings":{"IPAddress":"127.0.0.1"}}`
		s := newFakeEngineServer(t, inspect, `{}`)
		rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/rcon", a, `{"cmd":"status"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "rcon_port") {
			t.Fatalf("want 400 mentioning rcon_port, got %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("no password env", func(t *testing.T) {
		inspect := `{"Name":"/x","State":{"Running":true},
			"Config":{"Labels":{"ferrous.managed":"true","ferrous.rcon_port":"28016"},"Env":["PATH=/bin"]},
			"NetworkSettings":{"IPAddress":"127.0.0.1"}}`
		s := newFakeEngineServer(t, inspect, `{}`)
		rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/rcon", a, `{"cmd":"status"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "RCON_PASSWORD") {
			t.Fatalf("want 400 mentioning RCON_PASSWORD, got %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("not running answers instantly with reason", func(t *testing.T) {
		inspect := `{"Name":"/x","State":{"Status":"exited","Running":false},
			"Config":{"Labels":{"ferrous.managed":"true","ferrous.rcon_port":"28016"},"Env":["RCON_PASSWORD=p"]},
			"NetworkSettings":{"IPAddress":""}}`
		s := newFakeEngineServer(t, inspect, `{}`)
		rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/rcon", a, `{"cmd":"status"}`)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not running") {
			t.Fatalf("want 503 not-running, got %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown container 404", func(t *testing.T) {
		inspect := `{"message":"No such container: abc"}`
		s := newFakeEngineServer(t, inspect, `{}`)
		rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/rcon", a, `{"cmd":"status"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestRconServerDownIs503Not500(t *testing.T) {
	// managed + capable, but rcon port nobody listens on → hub can't connect
	inspect := `{"Name":"/x","State":{"Running":true},
		"Config":{"Labels":{"ferrous.managed":"true","ferrous.rcon_port":"1"},"Env":["RCON_PASSWORD=p"]},
		"NetworkSettings":{"IPAddress":"127.0.0.1"}}`
	s := newFakeEngineServer(t, inspect, `{}`)
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc/rcon",
		"Bearer "+testToken, `{"cmd":"status","timeout_ms":800}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rcon not connected") {
		t.Fatalf("friendly message expected: %s", rec.Body.String())
	}
}

func TestPruneHubsOnListing(t *testing.T) {
	fake := startFakeRcon(t)
	list := `[{"Id":"abc123","Names":["/fake-server"],"Image":"img","State":"running","Status":"Up","Labels":{"ferrous.managed":"true","ferrous.rcon_port":"28016"}}]`
	s := newFakeEngineServer(t, rconInspect(rconPort(t, fake), "", ""), list)

	// create a hub for a container that will NOT be in the listing
	s.hubFor("ghost-container-id")
	if len(s.hubs) != 1 {
		t.Fatalf("hub not registered: %d", len(s.hubs))
	}
	rec := do(t, s, http.MethodGet, "/api/v1/servers", "Bearer "+testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("listing failed: %d", rec.Code)
	}
	// prune runs before the response is written
	s.hubsMu.Lock()
	n := len(s.hubs)
	s.hubsMu.Unlock()
	if n != 0 {
		t.Fatalf("stale hub not pruned (still %d)", n)
	}
	// listing shows the rcon capability flag
	if !strings.Contains(rec.Body.String(), `"rcon":true`) {
		t.Fatalf("servers list must carry rcon capability: %s", rec.Body.String())
	}
	// listing carries the agent build version (panel's skew check reads it)
	if !strings.Contains(rec.Body.String(), `"version":"test"`) {
		t.Fatalf("servers list must carry the agent version: %s", rec.Body.String())
	}
}

func TestRconConcurrencyThroughHTTP(t *testing.T) {
	fake := startFakeRcon(t)
	s := newFakeEngineServer(t, rconInspect(rconPort(t, fake), "", ""), `{}`)
	a := "Bearer " + testToken
	const n = 6
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			cmd := fmt.Sprintf("say worker-%d", i)
			rec := doBody(t, s, http.MethodPost, "/api/v1/servers/abc123/rcon", a,
				fmt.Sprintf(`{"cmd":%q}`, cmd))
			if rec.Code != http.StatusOK {
				errs <- fmt.Sprintf("worker %d: %d %s", i, rec.Code, rec.Body.String())
				return
			}
			want := "Say: worker-" + fmt.Sprint(i)
			if !strings.Contains(rec.Body.String(), want) {
				errs <- fmt.Sprintf("worker %d: correlation mixup in %s", i, rec.Body.String())
				return
			}
			errs <- ""
		}(i)
	}
	for i := 0; i < n; i++ {
		if msg := <-errs; msg != "" {
			t.Error(msg)
		}
	}
}
