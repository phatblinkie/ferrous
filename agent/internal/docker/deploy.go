package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Phase 5: deployment — pull image (if missing) → create container → start.
// The spec is deliberately constrained (not a raw engine create payload):
// the agent adds the ferrous labels, a restart policy and the data bind, so
// a fat-fingered wizard submission can't produce a privileged container.

// ErrNameConflict maps to HTTP 409 (a container with that name exists).
var ErrNameConflict = errors.New("a container with that name already exists")

// PullError is a failed image pull (bad name/tag, registry unreachable —
// the engine streams these as 200 + {"error": ...} lines, not HTTP errors).
type PullError struct{ Msg string }

func (e *PullError) Error() string { return "image pull failed: " + e.Msg }

type DeployPort struct {
	Container int    `json:"container"`
	Host      int    `json:"host"`
	Proto     string `json:"proto"` // tcp | udp
}

// DeploySpec is the panel→agent deployment request (all fields validated).
type DeploySpec struct {
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	Command       []string          `json:"command,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Ports         []DeployPort      `json:"ports,omitempty"`
	DataDir       string            `json:"data_dir"`
	ContainerPath string            `json:"container_path,omitempty"` // default /server
	Labels        map[string]string `json:"labels,omitempty"`         // extra ferrous.* labels
	RconPort      int               `json:"rcon_port,omitempty"`      // 0 = no ferrous.rcon_port label
	MemoryMB      int               `json:"memory_mb,omitempty"`      // 0 = unlimited
}

// DeployResult reports what happened.
type DeployResult struct {
	Id      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Pulled  bool   `json:"pulled"`
	Started bool   `json:"started"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	lblKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// Validate checks the whole spec; all problems are reported at once.
func (s *DeploySpec) Validate() error {
	var errs []error
	if !nameRe.MatchString(s.Name) {
		errs = append(errs, fmt.Errorf("invalid name %q (want letters/digits/_.- starting alphanumeric)", s.Name))
	}
	img := s.Image
	if img == "" || len(img) > 256 || strings.ContainsAny(img, " \t\n") || strings.HasPrefix(img, "-") {
		errs = append(errs, fmt.Errorf("invalid image %q", s.Image))
	}
	if s.DataDir == "" || !isAbsPath(s.DataDir) || s.DataDir == "/" {
		errs = append(errs, fmt.Errorf("data_dir must be an absolute path (not /): %q", s.DataDir))
	}
	cpath := s.ContainerPath
	if cpath == "" {
		cpath = "/server"
	}
	if !isAbsPath(cpath) || cpath == "/" {
		errs = append(errs, fmt.Errorf("container_path must be an absolute path (not /): %q", s.ContainerPath))
	}
	if len(s.Env) > 64 {
		errs = append(errs, fmt.Errorf("too many env vars (%d > 64)", len(s.Env)))
	}
	total := 0
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("invalid env key %q", k))
		}
		if len(v) > 4096 {
			errs = append(errs, fmt.Errorf("env %s value too long (%d > 4096)", k, len(v)))
		}
		total += len(k) + len(v)
	}
	if total > 16384 {
		errs = append(errs, fmt.Errorf("env too large (%d > 16384 bytes)", total))
	}
	if len(s.Ports) > 16 {
		errs = append(errs, fmt.Errorf("too many ports (%d > 16)", len(s.Ports)))
	}
	for i, p := range s.Ports {
		if p.Container < 1 || p.Container > 65535 || p.Host < 1 || p.Host > 65535 {
			errs = append(errs, fmt.Errorf("port %d: out of range", i))
		}
		if p.Proto != "tcp" && p.Proto != "udp" {
			errs = append(errs, fmt.Errorf("port %d: proto must be tcp or udp", i))
		}
	}
	for k, v := range s.Labels {
		if !strings.HasPrefix(k, "ferrous.") || !lblKeyRe.MatchString(strings.TrimPrefix(k, "ferrous.")) {
			errs = append(errs, fmt.Errorf("label %q: keys must be ferrous.<lowercase>", k))
		}
		if len(v) > 255 {
			errs = append(errs, fmt.Errorf("label %s value too long", k))
		}
	}
	if s.RconPort < 0 || s.RconPort > 65535 {
		errs = append(errs, fmt.Errorf("rcon_port out of range: %d", s.RconPort))
	}
	if s.MemoryMB != 0 && (s.MemoryMB < 256 || s.MemoryMB > 1<<20) {
		errs = append(errs, fmt.Errorf("memory_mb out of range (256..1048576): %d", s.MemoryMB))
	}
	if len(s.Command) > 32 {
		errs = append(errs, fmt.Errorf("command too long (%d > 32 args)", len(s.Command)))
	}
	for i, a := range s.Command {
		if len(a) > 1024 {
			errs = append(errs, fmt.Errorf("command arg %d too long", i))
		}
	}
	return errors.Join(errs...)
}

func isAbsPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.ContainsRune(p, 0)
}

// containerName returns the engine's own name form ("/name").
func (s *DeploySpec) containerName() string { return "/" + s.Name }

// CreatePayload is the /containers/create body we send.
type CreatePayload struct {
	Image        string            `json:"Image"`
	Cmd          []string          `json:"Cmd,omitempty"`
	Env          []string          `json:"Env,omitempty"`
	Labels       map[string]string `json:"Labels"`
	ExposedPorts map[string]any    `json:"ExposedPorts,omitempty"`
	HostConfig   *HostConfig       `json:"HostConfig"`
}

type HostConfig struct {
	Binds         []string                 `json:"Binds,omitempty"`
	PortBindings  map[string][]PortBinding `json:"PortBindings,omitempty"`
	Memory        int64                    `json:"Memory,omitempty"`
	RestartPolicy struct{ Name string }    `json:"RestartPolicy"`
}

type PortBinding struct {
	HostIP   string `json:"HostIP"`
	HostPort string `json:"HostPort"`
}

// payload builds the create request. Deterministic (sorted env) for tests.
func (s *DeploySpec) payload() *CreatePayload {
	cpath := s.ContainerPath
	if cpath == "" {
		cpath = "/server"
	}
	p := &CreatePayload{
		Image:  s.Image,
		Cmd:    s.Command,
		Labels: map[string]string{},
		HostConfig: &HostConfig{
			Binds: []string{s.DataDir + ":" + cpath + ":rw"},
		},
	}
	p.HostConfig.RestartPolicy.Name = "unless-stopped"
	if s.MemoryMB > 0 {
		p.HostConfig.Memory = int64(s.MemoryMB) << 20
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p.Env = append(p.Env, k+"="+s.Env[k])
	}
	if len(s.Ports) > 0 {
		p.ExposedPorts = map[string]any{}
		p.HostConfig.PortBindings = map[string][]PortBinding{}
		for _, pt := range s.Ports {
			key := strconv.Itoa(pt.Container) + "/" + pt.Proto
			p.ExposedPorts[key] = struct{}{}
			p.HostConfig.PortBindings[key] = []PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(pt.Host)}}
		}
	}
	// ferrous labels: ours win over spec.Labels on the reserved keys
	for k, v := range s.Labels {
		p.Labels[k] = v
	}
	p.Labels["ferrous.managed"] = "true"
	p.Labels["ferrous.datadir"] = s.DataDir
	if s.RconPort > 0 {
		p.Labels["ferrous.rcon_port"] = strconv.Itoa(s.RconPort)
	}
	return p
}

// Deploy runs the full flow under ctx (the caller's deadline — pulls of
// multi-GB images take minutes). Steps:
//  1. ensure the data dir exists (host side, catches permission errors early)
//  2. image present? else pull (streamed; {"error":...} lines → *PullError)
//  3. create container (name conflict → ErrNameConflict)
//  4. start it (failures keep the container for inspection and name the id)
func (c *Client) Deploy(ctx context.Context, s *DeploySpec) (*DeployResult, error) {
	if err := os.MkdirAll(s.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data_dir: %w", err)
	}
	res := &DeployResult{Name: s.Name, Image: s.Image}

	exists, err := c.imageExists(ctx, s.Image)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := c.pull(ctx, s.Image); err != nil {
			return nil, err
		}
		res.Pulled = true
	}

	q := url.Values{"name": {s.Name}}
	var created struct{ Id string }
	if err := c.doJSON(ctx, http.MethodPost, "/containers/create?"+q.Encode(), s.payload(), &created); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusConflict {
			return nil, ErrNameConflict
		}
		return nil, err
	}
	res.Id = created.Id

	if err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.Id)+"/start", nil, nil); err != nil {
		return res, fmt.Errorf("container %.12s created but start failed: %w", created.Id, err)
	}
	res.Started = true
	return res, nil
}

func (c *Client) imageExists(ctx context.Context, image string) (bool, error) {
	err := c.doJSON(ctx, http.MethodGet, "/images/"+image+"/json", nil, nil)
	if err == nil {
		return true, nil
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

// pull streams /images/create. The engine answers 200 + JSON-lines progress
// ({"status":"Pulling fs layer"...}); an {"error":"..."} line fails the pull
// even though the HTTP status is 200 — that's how "manifest unknown" arrives.
func (c *Client) pull(ctx context.Context, image string) error {
	repo, tag := splitImage(image)
	q := url.Values{"fromImage": {repo}}
	if tag != "" {
		q.Set("tag", tag)
	}
	hdr := http.Header{"X-Registry-Auth": []string{"e30="}} // base64("{}") = anonymous pull
	res, err := c.raw(ctx, http.MethodPost, "/images/create?"+q.Encode(), hdr, nil)
	if err != nil {
		return &PullError{Msg: err.Error()}
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastErr string
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &m) == nil && m.Error != "" {
			lastErr = m.Error
		}
	}
	if err := sc.Err(); err != nil {
		return &PullError{Msg: err.Error()}
	}
	if lastErr != "" {
		return &PullError{Msg: lastErr}
	}
	return nil
}

// splitImage splits "repo:tag" (colon only counts past the last slash so
// "localhost:5000/x" stays intact); digests pass through whole.
func splitImage(image string) (repo, tag string) {
	if strings.Contains(image, "@") {
		return image, ""
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}
