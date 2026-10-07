// Package docker is a minimal Docker Engine REST API client over the unix
// socket. Stdlib only by design — the agent must stay a static, dependency-free
// binary. It deliberately exposes a narrow surface: version/info, container
// discovery, power actions (phase 2), log streaming (phase 2).
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

type Client struct {
	hc   *http.Client
	base string
}

// New returns a client talking to the engine at socket (e.g. /var/run/docker.sock).
// There is no global client timeout: requests carry their own deadlines via
// context so that future log streams can run long-lived.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{hc: &http.Client{Transport: tr}, base: "http://docker"}
}

// APIError is a non-2xx answer from the engine.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker api %d: %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	return c.doJSON(ctx, method, path, nil, out)
}

// doJSON sends an optional JSON body and decodes a JSON reply. A nil out
// drains the (small) response so the connection can be reused.
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	res, err := c.raw(ctx, method, path, nil, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// raw performs a request with an optional raw body and extra headers,
// returning the response for 2xx (caller closes). Non-2xx becomes *APIError.
// This is what image pulls use: they stream a JSON-lines progress body that
// must be read to EOF under the caller's context deadline (minutes for big
// images — there is intentionally no global client timeout).
func (c *Client) raw(ctx context.Context, method, path string, hdr http.Header, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker unreachable: %w", err)
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		return nil, apiErrorFrom(res)
	}
	return res, nil
}

// apiErrorFrom builds a typed error from a non-2xx engine response body.
func apiErrorFrom(res *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Message == "" {
		e.Message = string(b)
	}
	return &APIError{Status: res.StatusCode, Message: e.Message}
}

// Ping checks engine liveness (GET /_ping → "OK").
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil)
}

// Version returns engine build/version details (GET /version).
func (c *Client) Version(ctx context.Context) (*Version, error) {
	var v Version
	if err := c.do(ctx, http.MethodGet, "/version", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Info returns runtime host info (GET /info).
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	if err := c.do(ctx, http.MethodGet, "/info", &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// ListContainers lists containers. label, when non-empty, is a docker label
// filter such as "ferrous.managed=true". all=true includes stopped containers.
func (c *Client) ListContainers(ctx context.Context, all bool, label string) ([]Container, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	if label != "" {
		// Engine API takes filters as a JSON map: {"label": {"key=value": true}}
		f, err := json.Marshal(map[string]map[string]bool{"label": {label: true}})
		if err != nil {
			return nil, err
		}
		q.Set("filters", string(f))
	}
	var out []Container
	if err := c.do(ctx, http.MethodGet, "/containers/json?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- engine payload types (subset we consume) -------------------------------

type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	GoVersion  string `json:"GoVersion"`
	OS         string `json:"Os"`
	Arch       string `json:"Arch"`
	BuildTime  string `json:"BuildTime"`
}

type Info struct {
	Name              string `json:"Name"`
	ServerVersion     string `json:"ServerVersion"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersPaused  int    `json:"ContainersPaused"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
	OSType            string `json:"OSType"`
	Driver            string `json:"Driver"`
}

type Container struct {
	Id      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Ports   []Port            `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
}

type Port struct {
	IP          string `json:"IP"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort"`
	Type        string `json:"Type"`
}
