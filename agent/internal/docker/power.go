package docker

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// ContainerDetail is the subset of /containers/{id}/json we consume.
type ContainerDetail struct {
	Name  string `json:"Name"` // leading slash: "/ferrous-x"
	State struct {
		Status    string `json:"Status"` // created|running|exited|...
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Tty    bool              `json:"Tty"`
		Labels map[string]string `json:"Labels"`
		Env    []string          `json:"Env"`
	} `json:"Config"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Networks  map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Inspect returns container detail; unknown ids surface as *APIError{404}.
func (c *Client) Inspect(ctx context.Context, id string) (*ContainerDetail, error) {
	var d ContainerDetail
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Power actions. The engine answers 304 Not Modified when the container is
// already in the requested state — 304 < 400 so do() treats it as success
// (power stays idempotent).

func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
}

// Stop asks the engine for a graceful shutdown: SIGTERM, then SIGKILL after
// graceSec seconds (Rust needs headroom to save — default grace is applied
// by the caller).
func (c *Client) Stop(ctx context.Context, id string, graceSec int) error {
	return c.do(ctx, http.MethodPost,
		"/containers/"+url.PathEscape(id)+"/stop?t="+strconv.Itoa(graceSec), nil)
}

func (c *Client) Restart(ctx context.Context, id string, graceSec int) error {
	return c.do(ctx, http.MethodPost,
		"/containers/"+url.PathEscape(id)+"/restart?t="+strconv.Itoa(graceSec), nil)
}
