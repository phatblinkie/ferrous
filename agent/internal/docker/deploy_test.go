package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- spec validation --------------------------------------------------------

func validSpec(t *testing.T) *DeploySpec {
	t.Helper()
	s := &DeploySpec{
		Name:     "rust-1",
		Image:    "ferrous/rustserver:latest",
		DataDir:  t.TempDir(),
		Env:      map[string]string{"RCON_PASSWORD": "pw", "SERVER_NAME": "My Server"},
		Ports:    []DeployPort{{Container: 28015, Host: 28015, Proto: "udp"}},
		RconPort: 28016,
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	return s
}

func TestValidateMatrix(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*DeploySpec)
		want string
	}{
		{"empty name", func(s *DeploySpec) { s.Name = "" }, "invalid name"},
		{"space in name", func(s *DeploySpec) { s.Name = "has space" }, "invalid name"},
		{"leading dash", func(s *DeploySpec) { s.Name = "-x" }, "invalid name"},
		{"name too long", func(s *DeploySpec) { s.Name = strings.Repeat("a", 70) }, "invalid name"},
		{"empty image", func(s *DeploySpec) { s.Image = "" }, "invalid image"},
		{"space in image", func(s *DeploySpec) { s.Image = "a b" }, "invalid image"},
		{"relative data_dir", func(s *DeploySpec) { s.DataDir = "srv/x" }, "absolute path"},
		{"root data_dir", func(s *DeploySpec) { s.DataDir = "/" }, "absolute path"},
		{"bad container_path", func(s *DeploySpec) { s.ContainerPath = "server" }, "container_path"},
		{"root container_path", func(s *DeploySpec) { s.ContainerPath = "/" }, "container_path"},
		{"bad env key", func(s *DeploySpec) { s.Env["1BAD"] = "x" }, "invalid env key"},
		{"huge env value", func(s *DeploySpec) { s.Env["K"] = strings.Repeat("v", 5000) }, "too long"},
		{"bad proto", func(s *DeploySpec) { s.Ports[0].Proto = "icmp" }, "tcp or udp"},
		{"port 0", func(s *DeploySpec) { s.Ports[0].Container = 0 }, "out of range"},
		{"host port too big", func(s *DeploySpec) { s.Ports[0].Host = 70000 }, "out of range"},
		{"too many ports", func(s *DeploySpec) {
			for i := 0; i < 17; i++ {
				s.Ports = append(s.Ports, DeployPort{Container: i + 1, Host: i + 1, Proto: "tcp"})
			}
		}, "too many ports"},
		{"non-ferrous label", func(s *DeploySpec) { s.Labels = map[string]string{"com.x": "1"} }, "ferrous."},
		{"uppercase label", func(s *DeploySpec) { s.Labels = map[string]string{"ferrous.Foo": "1"} }, "ferrous."},
		{"rcon port high", func(s *DeploySpec) { s.RconPort = 70000 }, "rcon_port"},
		{"memory tiny", func(s *DeploySpec) { s.MemoryMB = 64 }, "memory_mb"},
		{"too many args", func(s *DeploySpec) {
			for i := 0; i < 33; i++ {
				s.Command = append(s.Command, "a")
			}
		}, "command too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec(t)
			tc.mut(s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// --- payload ----------------------------------------------------------------

func TestPayloadContract(t *testing.T) {
	s := validSpec(t)
	s.Labels = map[string]string{"ferrous.note": "hi", "ferrous.managed": "false"}
	s.Command = []string{"/entrypoint.sh"}
	s.MemoryMB = 2048
	p := s.payload()

	if p.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart policy: %q", p.HostConfig.RestartPolicy.Name)
	}
	wantBind := s.DataDir + ":/server:rw"
	if len(p.HostConfig.Binds) != 1 || p.HostConfig.Binds[0] != wantBind {
		t.Errorf("binds: %v", p.HostConfig.Binds)
	}
	if p.HostConfig.Memory != 2048<<20 {
		t.Errorf("memory: %d", p.HostConfig.Memory)
	}
	// labels: ferrous.managed forced true despite spec, datadir set, rcon port set
	if p.Labels["ferrous.managed"] != "true" || p.Labels["ferrous.datadir"] != s.DataDir ||
		p.Labels["ferrous.rcon_port"] != "28016" || p.Labels["ferrous.note"] != "hi" {
		t.Errorf("labels: %v", p.Labels)
	}
	// ports
	key := "28015/udp"
	if _, ok := p.ExposedPorts[key]; !ok {
		t.Errorf("exposed ports: %v", p.ExposedPorts)
	}
	b := p.HostConfig.PortBindings[key]
	if len(b) != 1 || b[0].HostPort != "28015" {
		t.Errorf("port bindings: %v", b)
	}
	// env sorted, KEY=VAL
	if len(p.Env) != 2 || p.Env[0] != "RCON_PASSWORD=pw" || p.Env[1] != "SERVER_NAME=My Server" {
		t.Errorf("env: %v", p.Env)
	}
	// rcon_port=0 → no label
	s.RconPort = 0
	if _, ok := s.payload().Labels["ferrous.rcon_port"]; ok {
		t.Error("rcon_port=0 must not create the label")
	}
}

// --- fake engine ------------------------------------------------------------

type engineRec struct {
	pullQuery  string
	pullAuth   string
	createPath string
	create     CreatePayload
	started    string
}

// fakeEngine serves the deploy endpoints over a unix socket and records calls.
func fakeEngine(t *testing.T, imageExists bool, pullErr string, createStatus, startStatus int) (*Client, *engineRec) {
	t.Helper()
	rec := &engineRec{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			if !imageExists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"No such image"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Id":"sha256:img"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/images/create":
			rec.pullQuery = r.URL.RawQuery
			rec.pullAuth = r.Header.Get("X-Registry-Auth")
			w.WriteHeader(http.StatusOK)
			if pullErr != "" {
				_, _ = w.Write([]byte(`{"errorDetail":{"message":"` + pullErr + `"},"error":"` + pullErr + `"}` + "\n"))
				return
			}
			_, _ = w.Write([]byte(`{"status":"Pulling fs layer"}` + "\n" + `{"status":"Download complete"}` + "\n"))
		case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
			rec.createPath = r.URL.RawQuery
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &rec.create)
			if createStatus >= 400 {
				w.WriteHeader(createStatus)
				_, _ = w.Write([]byte(`{"message":"Conflict. The container name is already in use"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Id":"` + strings.Repeat("ab", 32) + `","Warnings":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			rec.started = r.URL.Path
			if startStatus >= 400 {
				w.WriteHeader(startStatus)
				_, _ = w.Write([]byte(`{"message":"port is already allocated"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found: ` + r.Method + " " + r.URL.Path + `"}`))
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
	return New(sock), rec
}

func TestDeployHappyPathPullsCreatesStarts(t *testing.T) {
	c, rec := fakeEngine(t, false, "", 200, 204)
	s := validSpec(t)

	res, err := c.Deploy(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pulled || !res.Started || res.Id == "" || res.Name != "rust-1" {
		t.Fatalf("result: %+v", res)
	}
	// pull split repo/tag + anonymous auth header
	if rec.pullQuery != "fromImage=ferrous%2Frustserver&tag=latest" {
		t.Errorf("pull query: %q", rec.pullQuery)
	}
	if rec.pullAuth != "e30=" {
		t.Errorf("pull auth: %q", rec.pullAuth)
	}
	// create name + payload recorded; start called with the created id
	if rec.createPath != "name=rust-1" {
		t.Errorf("create path: %q", rec.createPath)
	}
	if rec.create.Labels["ferrous.managed"] != "true" {
		t.Errorf("create labels: %v", rec.create.Labels)
	}
	if !strings.HasSuffix(rec.started, "/"+res.Id+"/start") {
		t.Errorf("start path: %q", rec.started)
	}
	// data dir was created
	if st, err := os.Stat(s.DataDir); err != nil || !st.IsDir() {
		t.Errorf("data_dir not created: %v", err)
	}
}

func TestDeploySkipsPullWhenImageExists(t *testing.T) {
	c, rec := fakeEngine(t, true, "", 200, 204)
	res, err := c.Deploy(context.Background(), validSpec(t))
	if err != nil || res.Pulled {
		t.Fatalf("pulled=%v err=%v", res.Pulled, err)
	}
	if rec.pullQuery != "" {
		t.Errorf("pull should be skipped, got query %q", rec.pullQuery)
	}
}

func TestDeployPullFailure(t *testing.T) {
	c, rec := fakeEngine(t, false, "manifest unknown", 200, 204)
	_, err := c.Deploy(context.Background(), validSpec(t))
	var pe *PullError
	if !errors.As(err, &pe) || !strings.Contains(pe.Msg, "manifest unknown") {
		t.Fatalf("want PullError, got %v", err)
	}
	if rec.createPath != "" {
		t.Error("must not create after failed pull")
	}
}

func TestDeployNameConflict(t *testing.T) {
	c, _ := fakeEngine(t, true, "", http.StatusConflict, 204)
	_, err := c.Deploy(context.Background(), validSpec(t))
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("want ErrNameConflict, got %v", err)
	}
}

func TestDeployStartFailureKeepsContainer(t *testing.T) {
	c, _ := fakeEngine(t, true, "", 200, http.StatusInternalServerError)
	res, err := c.Deploy(context.Background(), validSpec(t))
	if err == nil || res == nil || res.Id == "" {
		t.Fatalf("want error + partial result with id, got res=%+v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "start failed") || !strings.Contains(err.Error(), "port is already allocated") {
		t.Fatalf("unhelpful error: %v", err)
	}
	if res.Started {
		t.Error("started must be false")
	}
}

func TestSplitImage(t *testing.T) {
	for _, tc := range []struct{ in, repo, tag string }{
		{"ubuntu", "ubuntu", "latest"},
		{"ubuntu:24.04", "ubuntu", "24.04"},
		{"ferrous/rustserver:latest", "ferrous/rustserver", "latest"},
		{"localhost:5000/x", "localhost:5000/x", "latest"}, // colon before slash = registry
		{"localhost:5000/x:v1", "localhost:5000/x", "v1"},
		{"repo@sha256:abc", "repo@sha256:abc", ""},
	} {
		repo, tag := splitImage(tc.in)
		if repo != tc.repo || tag != tc.tag {
			t.Errorf("%q → %q,%q want %q,%q", tc.in, repo, tag, tc.repo, tc.tag)
		}
	}
}
