package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ferrous/agent/internal/docker"
	"ferrous/agent/internal/rcon"
)

// --- hub management ---------------------------------------------------------

// rconResolver re-derives the endpoint from docker on every (re)connect —
// container IPs move on restart, and a vanished container must stop the loop
// with a clear reason ("container is gone" / "no such container").
func (s *Server) rconResolver(id string) rcon.Resolver {
	return func(ctx context.Context) (rcon.Config, error) {
		rc, err := s.docker.RconConfig(ctx, id)
		if err != nil {
			var ae *docker.APIError
			if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
				return rcon.Config{}, errors.New("container is gone")
			}
			return rcon.Config{}, err
		}
		return rcon.Config{Addr: rc.Addr, Password: rc.Password}, nil
	}
}

// hubFor returns the per-container hub, creating it on first use.
func (s *Server) hubFor(id string) *rcon.Hub {
	s.hubsMu.Lock()
	defer s.hubsMu.Unlock()
	if h, ok := s.hubs[id]; ok {
		return h
	}
	h := rcon.New(shortID(id), s.rconResolver(id), rcon.Options{
		Log: s.log.With("component", "rcon", "container", shortID(id)),
	})
	s.hubs[id] = h
	return h
}

// Close shuts every rcon hub down (agent shutdown / tests). Async: hub.Close
// can block on an in-flight dial, so callers shouldn't wait on request paths.
func (s *Server) Close() {
	s.hubsMu.Lock()
	dead := make([]*rcon.Hub, 0, len(s.hubs))
	for id, h := range s.hubs {
		delete(s.hubs, id)
		dead = append(dead, h)
	}
	s.hubsMu.Unlock()
	var wg sync.WaitGroup
	for _, h := range dead {
		wg.Add(1)
		go func(h *rcon.Hub) {
			defer wg.Done()
			h.Close()
		}(h)
	}
	wg.Wait()
}

// pruneHubs closes hubs whose container disappeared from the latest listing.
// Close runs async: it can block on an in-flight dial for a few seconds and
// must not stall the listing request.
func (s *Server) pruneHubs(list []docker.Container) {
	alive := make(map[string]bool, len(list))
	for _, c := range list {
		alive[c.Id] = true
	}
	s.hubsMu.Lock()
	var dead []*rcon.Hub
	for id, h := range s.hubs {
		if !alive[id] {
			delete(s.hubs, id)
			dead = append(dead, h)
		}
	}
	s.hubsMu.Unlock()
	for _, h := range dead {
		go h.Close()
	}
}

// --- gate -------------------------------------------------------------------

// rconGate: valid id → inspect (404/502) → managed (403) → capability
// (400: rcon_port label, then RCON_PASSWORD env). Returns false after writing
// the error; on true the caller owns a fresh inspect of the container.
func (s *Server) rconGate(w http.ResponseWriter, r *http.Request, id string) (*docker.ContainerDetail, bool) {
	d, err := s.docker.Inspect(r.Context(), id)
	if err != nil {
		writeDockerError(w, err)
		return nil, false
	}
	if d.Config.Labels["ferrous.managed"] != "true" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a ferrous-managed container"})
		return nil, false
	}
	if _, ok := d.Config.Labels["ferrous.rcon_port"]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "container has no ferrous.rcon_port label (not a ferrous game server?)"})
		return nil, false
	}
	if pw, ok := docker.EnvValue(d.Config.Env, "RCON_PASSWORD"); !ok || pw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "container has no RCON_PASSWORD env (rcon unavailable)"})
		return nil, false
	}
	return d, true
}

// --- endpoints --------------------------------------------------------------

// handleRcon: POST /api/v1/servers/{id}/rcon  {"cmd":"status","timeout_ms":8000}
// Status contract the panel depends on:
//
//	200 {"ok":true,"out","ms"}  · 400 bad cmd/config  · 403 unmanaged
//	503 not connected / conn lost (server booting, restarting, stopped)
//	504 no reply in time — on Rust this also means "command does not exist"
//	     (vanilla answers nothing to unknown rcon commands)
func (s *Server) handleRcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server id"})
		return
	}
	var req struct {
		Cmd       string `json:"cmd"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `bad request body (want {"cmd": ...})`})
		return
	}
	cmd := strings.TrimSpace(req.Cmd)
	if cmd == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty command"})
		return
	}
	if len(cmd) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command too long (max 500)"})
		return
	}
	timeout := 8 * time.Second
	if req.TimeoutMS != 0 {
		if req.TimeoutMS < 500 || req.TimeoutMS > 30000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "timeout_ms must be 500..30000"})
			return
		}
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	d, ok := s.rconGate(w, r, id)
	if !ok {
		return
	}
	// stopped container: answer immediately with the reason instead of letting
	// the hub retry-loop it into a generic "not connected"
	if !d.State.Running {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "rcon not connected (container not running)"})
		return
	}

	t0 := time.Now()
	out, err := s.hubFor(id).Send(r.Context(), cmd, timeout)
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		status := http.StatusServiceUnavailable // default: not connected / conn lost
		if errors.Is(err, rcon.ErrTimeout) {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "ms": ms})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "out": out, "ms": ms})
}

// handleRconStatus: GET /api/v1/servers/{id}/rcon — hub diagnostics for the
// panel (never exposes the password or the resolved address).
func (s *Server) handleRconStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server id"})
		return
	}
	if _, ok := s.rconGate(w, r, id); !ok {
		return
	}
	connected, lastErr, pushes := s.hubFor(id).Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": connected,
		"error":     lastErr,
		"pushes":    pushes, // Identifier:0 messages seen (and discarded)
		"time":      now(),
	})
}
