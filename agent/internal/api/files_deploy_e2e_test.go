package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ferrous/agent/internal/docker"
)

// --- fixtures ---------------------------------------------------------------

// filesInspect: managed container whose data dir is the ferrous.datadir label.
func filesInspect(dataDir string) string {
	return fmt.Sprintf(`{
		"Name": "/fs-server",
		"State": {"Status": "running", "Running": true},
		"Config": {"Labels": {"ferrous.managed": "true", "ferrous.datadir": %q}},
		"Mounts": [],
		"NetworkSettings": {"IPAddress": "127.0.0.1"}
	}`, dataDir)
}

// mountsInspect: managed container without the label — root comes from the
// single writable mount.
func mountsInspect(dataDir string) string {
	return fmt.Sprintf(`{
		"Name": "/fs-mount",
		"State": {"Status": "running", "Running": true},
		"Config": {"Labels": {"ferrous.managed": "true"}},
		"Mounts": [{"Type": "bind", "Source": %q, "Destination": "/server", "RW": true}],
		"NetworkSettings": {"IPAddress": "127.0.0.1"}
	}`, dataDir)
}

// rootlessInspect: managed but no label and no mounts → files must 400.
const rootlessInspect = `{
	"Name": "/rootless",
	"State": {"Status": "running", "Running": true},
	"Config": {"Labels": {"ferrous.managed": "true"}},
	"Mounts": [],
	"NetworkSettings": {"IPAddress": "127.0.0.1"}
}`

// unmanagedInspect: foreign container → 403 on item endpoints.
const unmanagedInspect = `{
	"Name": "/foreign",
	"State": {"Status": "running", "Running": true},
	"Config": {"Labels": {}},
	"Mounts": [{"Type": "bind", "Source": "/srv/x", "Destination": "/data", "RW": true}],
	"NetworkSettings": {"IPAddress": "127.0.0.1"}
}`

func seedDataDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server/main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "oxide/plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server/main/server.cfg"),
		[]byte("server.name \"ferrous test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oxide/plugins/InvDump.dll"),
		[]byte{0x4d, 0x5a, 0x00, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// --- files ------------------------------------------------------------------

func TestFilesEndToEnd(t *testing.T) {
	root := seedDataDir(t)
	s := newFakeEngineServer(t, filesInspect(root), `{}`)
	a := "Bearer " + testToken
	base := "/api/v1/servers/abc123/files"

	// root listing: dirs first
	rec := do(t, s, http.MethodGet, base, a)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var fi docker.FileInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &fi); err != nil {
		t.Fatal(err)
	}
	if fi.Type != "dir" || len(fi.Entries) != 2 || fi.Entries[0].Name != "oxide" {
		t.Fatalf("root listing: %+v", fi)
	}

	// text file: utf8 content
	rec = do(t, s, http.MethodGet, base+"?path=server/main/server.cfg", a)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"encoding":"utf8"`) ||
		!strings.Contains(rec.Body.String(), `server.name \"ferrous test\"`) {
		t.Fatalf("text get: %d %s", rec.Code, rec.Body.String())
	}

	// binary file: base64 roundtrip
	rec = do(t, s, http.MethodGet, base+"?path=oxide/plugins/InvDump.dll", a)
	if rec.Code != http.StatusOK {
		t.Fatalf("binary get: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fi); err != nil {
		t.Fatal(err)
	}
	if fi.Encoding != "base64" {
		t.Fatalf("binary encoding: %q", fi.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(fi.Content)
	if err != nil || len(raw) != 4 || raw[0] != 0x4d {
		t.Fatalf("base64 payload: %v %v", raw, err)
	}

	// write then read back
	enc := base64.StdEncoding.EncodeToString([]byte("hello ferrous"))
	rec = doBody(t, s, http.MethodPut, base, a,
		fmt.Sprintf(`{"path":"oxide/plugins/New.dll","content":%q,"encoding":"base64"}`, enc))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, http.MethodGet, base+"?path=oxide/plugins/New.dll", a)
	if !strings.Contains(rec.Body.String(), "hello ferrous") {
		t.Fatalf("readback: %s", rec.Body.String())
	}
	// no temp litter
	entries, _ := os.ReadDir(filepath.Join(root, "oxide/plugins"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferrous-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}

	// traversal / absolute / missing / kind errors
	for _, tc := range []struct {
		q    string
		want int
	}{
		{"?path=../../etc/passwd", http.StatusBadRequest},
		{"?path=/etc/passwd", http.StatusBadRequest},
		{"?path=nope", http.StatusNotFound},
		{"?path=server/main", http.StatusOK}, // a dir lists fine
	} {
		rec = do(t, s, http.MethodGet, base+tc.q, a)
		if rec.Code != tc.want {
			t.Errorf("%s: want %d got %d (%s)", tc.q, tc.want, rec.Code, rec.Body.String())
		}
	}

	// PUT validation: bad encoding, wrong kind
	rec = doBody(t, s, http.MethodPut, base, a, `{"path":"x","content":"!!","encoding":"rot13"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad encoding: %d", rec.Code)
	}
	rec = doBody(t, s, http.MethodPut, base, a, `{"path":"server/main","content":"x","encoding":"utf8"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("write onto dir: %d %s", rec.Code, rec.Body.String())
	}

	// method + auth walls
	rec = doBody(t, s, http.MethodPost, base, a, `{}`)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT" {
		t.Errorf("405: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(t, s, http.MethodGet, base, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("401: %d", rec.Code)
	}
}

func TestFilesGatesAndRootFallback(t *testing.T) {
	a := "Bearer " + testToken
	base := "/api/v1/servers/abc123/files"

	// root from the single mount (no label)
	root := seedDataDir(t)
	s := newFakeEngineServer(t, mountsInspect(root), `{}`)
	rec := do(t, s, http.MethodGet, base, a)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "oxide") {
		t.Fatalf("mount fallback: %d %s", rec.Code, rec.Body.String())
	}

	// managed but no root anywhere → 400 with a label hint
	s = newFakeEngineServer(t, rootlessInspect, `{}`)
	rec = do(t, s, http.MethodGet, base, a)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ferrous.datadir") {
		t.Fatalf("rootless: %d %s", rec.Code, rec.Body.String())
	}

	// foreign container → 403 (files are as gated as rcon/power)
	s = newFakeEngineServer(t, unmanagedInspect, `{}`)
	rec = do(t, s, http.MethodGet, base, a)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unmanaged: %d %s", rec.Code, rec.Body.String())
	}

	// unknown container → 404
	s = newFakeEngineServer(t, `{"message":"No such container: abc123"}`, `{}`)
	rec = do(t, s, http.MethodGet, base, a)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body.String())
	}

	// invalid id → 400
	s = newFakeEngineServer(t, filesInspect(root), `{}`)
	rec = do(t, s, http.MethodGet, "/api/v1/servers/bad%20id/files", a)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: %d %s", rec.Code, rec.Body.String())
	}
}

// --- deploy engine fixture --------------------------------------------------

type deployEngineOpts struct {
	imageExists  bool
	pullErr      string
	createStatus int
	startStatus  int
}

type deployEngine struct {
	client     *docker.Client
	rec        deployEngineOpts
	pullQuery  string
	pullAuth   string
	createPath string
	create     docker.CreatePayload
	startPath  string
}

func newDeployEngine(t *testing.T, opts deployEngineOpts) *deployEngine {
	t.Helper()
	e := &deployEngine{rec: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
			if !opts.imageExists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"No such image: x"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Id":"sha256:img"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/images/create":
			e.pullQuery = r.URL.RawQuery
			e.pullAuth = r.Header.Get("X-Registry-Auth")
			if opts.pullErr != "" {
				_, _ = fmt.Fprintf(w, `{"errorDetail":{"message":%q},"error":%q}`+"\n", opts.pullErr, opts.pullErr)
				return
			}
			_, _ = w.Write([]byte(`{"status":"Pulling fs layer"}` + "\n"))
		case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
			e.createPath = r.URL.RawQuery
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &e.create)
			if opts.createStatus >= 400 {
				w.WriteHeader(opts.createStatus)
				_, _ = w.Write([]byte(`{"message":"Conflict. The container name rust-e2e is already in use"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Id":"` + strings.Repeat("cd", 32) + `"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			e.startPath = r.URL.Path
			if opts.startStatus >= 400 {
				w.WriteHeader(opts.startStatus)
				_, _ = w.Write([]byte(`{"message":"port is already allocated"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	})
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	e.client = docker.New(sock)
	return e
}

func deployServer(t *testing.T, e *deployEngine) *Server {
	t.Helper()
	s := New(e.client, testToken, "test", nil)
	t.Cleanup(s.Close)
	return s
}

const validDeployBody = `{
	"name": "rust-e2e",
	"image": "ferrous/rustserver:latest",
	"data_dir": "%s",
	"env": {"RCON_PASSWORD": "pw", "SERVER_NAME": "E2E"},
	"ports": [{"container": 28015, "host": 38015, "proto": "udp"}],
	"rcon_port": 28016,
	"memory_mb": 2048
}`

// --- deploy -----------------------------------------------------------------

func TestDeployEndToEnd(t *testing.T) {
	e := newDeployEngine(t, deployEngineOpts{}) // no image → pull path
	s := deployServer(t, e)
	a := "Bearer " + testToken
	dataDir := filepath.Join(t.TempDir(), "e2e-data")
	body := fmt.Sprintf(validDeployBody, dataDir)

	rec := doBody(t, s, http.MethodPost, "/api/v1/servers", a, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK  bool `json:"ok"`
		Res struct {
			Id      string `json:"id"`
			Pulled  bool   `json:"pulled"`
			Started bool   `json:"started"`
		} `json:"res"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !out.Res.Pulled || !out.Res.Started || out.Res.Id == "" {
		t.Fatalf("result: %+v", out)
	}

	if e.pullQuery != "fromImage=ferrous%2Frustserver&tag=latest" {
		t.Errorf("pull query: %q", e.pullQuery)
	}
	if e.pullAuth != "e30=" {
		t.Errorf("anonymous pull auth: %q", e.pullAuth)
	}
	if e.createPath != "name=rust-e2e" {
		t.Errorf("create name: %q", e.createPath)
	}
	if e.create.Labels["ferrous.managed"] != "true" ||
		e.create.Labels["ferrous.datadir"] != dataDir ||
		e.create.Labels["ferrous.rcon_port"] != "28016" {
		t.Errorf("labels: %v", e.create.Labels)
	}
	if len(e.create.HostConfig.Binds) != 1 || e.create.HostConfig.Binds[0] != dataDir+":/server:rw" {
		t.Errorf("binds: %v", e.create.HostConfig.Binds)
	}
	if e.create.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart: %+v", e.create.HostConfig.RestartPolicy)
	}
	if !strings.HasSuffix(e.startPath, "/"+out.Res.Id+"/start") {
		t.Errorf("start: %q", e.startPath)
	}
	if st, err := os.Stat(dataDir); err != nil || !st.IsDir() {
		t.Errorf("data_dir not created: %v", err)
	}
}

func TestDeploySkipsPullWhenImageLocal(t *testing.T) {
	e := newDeployEngine(t, deployEngineOpts{imageExists: true})
	s := deployServer(t, e)
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers", "Bearer "+testToken,
		fmt.Sprintf(validDeployBody, t.TempDir()))
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"pulled":false`) {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}
	if e.pullQuery != "" {
		t.Errorf("pull skipped: %q", e.pullQuery)
	}
}

func TestDeployValidationStopsBeforeEngine(t *testing.T) {
	e := newDeployEngine(t, deployEngineOpts{imageExists: true})
	s := deployServer(t, e)
	a := "Bearer " + testToken

	cases := []struct{ name, body string }{
		{"bad json", `not json`},
		{"empty body", ``},
		{"bad name", `{"name":"has space","image":"x","data_dir":"/srv/x"}`},
		{"relative data_dir", `{"name":"ok1","image":"x","data_dir":"srv/x"}`},
		{"root data_dir", `{"name":"ok1","image":"x","data_dir":"/"}`},
		{"bad env key", `{"name":"ok1","image":"x","data_dir":"/srv/x","env":{"1BAD":"v"}}`},
		{"bad proto", `{"name":"ok1","image":"x","data_dir":"/srv/x","ports":[{"container":1,"host":1,"proto":"icmp"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doBody(t, s, http.MethodPost, "/api/v1/servers", a, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if e.pullQuery != "" || e.createPath != "" {
		t.Fatalf("engine must not be touched: pull=%q create=%q", e.pullQuery, e.createPath)
	}
}

func TestDeployPullFailureIs400(t *testing.T) {
	e := newDeployEngine(t, deployEngineOpts{pullErr: "manifest unknown: ferrous/nope"})
	s := deployServer(t, e)
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers", "Bearer "+testToken,
		fmt.Sprintf(validDeployBody, t.TempDir()))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "image pull failed") {
		t.Fatalf("pull failure: %d %s", rec.Code, rec.Body.String())
	}
	if e.createPath != "" {
		t.Fatal("must not create after failed pull")
	}
}

func TestDeployNameConflictIs409(t *testing.T) {
	e := newDeployEngine(t, deployEngineOpts{imageExists: true, createStatus: http.StatusConflict})
	s := deployServer(t, e)
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers", "Bearer "+testToken,
		fmt.Sprintf(validDeployBody, t.TempDir()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestDeployStartFailureIs502NamingContainer(t *testing.T) {
	e := newDeployEngine(t, deployEngineOpts{imageExists: true, startStatus: http.StatusInternalServerError})
	s := deployServer(t, e)
	rec := doBody(t, s, http.MethodPost, "/api/v1/servers", "Bearer "+testToken,
		fmt.Sprintf(validDeployBody, t.TempDir()))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "start failed") ||
		!strings.Contains(rec.Body.String(), "port is already allocated") {
		t.Fatalf("unhelpful error: %s", rec.Body.String())
	}
}

func TestDeployWallsAnd405(t *testing.T) {
	e := newDeployEngine(t, deployEngineOpts{imageExists: true})
	s := deployServer(t, e)

	rec := doBody(t, s, http.MethodPost, "/api/v1/servers", "",
		fmt.Sprintf(validDeployBody, t.TempDir()))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("401: %d", rec.Code)
	}
	// DELETE /servers must say GET, POST — not 404
	rec = do(t, s, http.MethodDelete, "/api/v1/servers", "Bearer "+testToken)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, POST" {
		t.Errorf("405: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if e.createPath != "" {
		t.Error("engine untouched")
	}
}
