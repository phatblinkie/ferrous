package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// validID bounds {id} path params before they touch a docker URL.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// handlePower: POST /api/v1/servers/{id}/power  {"action":"start|stop|restart"}
// Optional query: grace=<seconds> (SIGTERM→SIGKILL window, 0..600, default 180 —
// Rust needs headroom to save, proven TimeoutStopSec=180; fast-exiting
// containers are unaffected since docker stop returns when the process exits).
func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server id"})
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `bad request body (want {"action": ...})`})
		return
	}
	switch req.Action {
	case "start", "stop", "restart":
	default:
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": fmt.Sprintf("unknown action %q (want start, stop or restart)", req.Action)})
		return
	}
	grace := 180 // Rust needs headroom to save before SIGKILL (proven 180 s)
	if g := r.URL.Query().Get("grace"); g != "" {
		n, err := strconv.Atoi(g)
		if err != nil || n < 0 || n > 600 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grace must be 0..600 seconds"})
			return
		}
		grace = n
	}

	// gate: managed containers only (the agent's surface stays narrow)
	d, err := s.docker.Inspect(r.Context(), id)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	if d.Config.Labels["ferrous.managed"] != "true" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a ferrous-managed container"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(grace)*time.Second+20*time.Second)
	defer cancel()
	switch req.Action {
	case "start":
		err = s.docker.Start(ctx, id)
	case "stop":
		err = s.docker.Stop(ctx, id, grace)
	case "restart":
		err = s.docker.Restart(ctx, id, grace)
	}
	if err != nil {
		writeDockerError(w, err)
		return
	}

	// best-effort state confirmation (omit on inspect hiccup — the action ran)
	state := ""
	sctx, scancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer scancel()
	if d2, ierr := s.docker.Inspect(sctx, id); ierr == nil {
		state = d2.State.Status
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "action": req.Action, "id": id, "state": state, "time": now(),
	})
}
