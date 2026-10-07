package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"ferrous/agent/internal/docker"
)

// deployTimeout bounds pull+create+start; multi-GB image pulls legitimately
// take minutes, but the request must not hang forever.
const deployTimeout = 15 * time.Minute

// handleDeploy: POST /api/v1/servers  — pull (if missing) → create → start.
//
// Status contract: 201 ok · 400 validation / pull failure / engine 4xx /
// data_dir trouble · 405 · 409 name conflict · 413 body too large ·
// 502 engine unreachable or start failure (message names the container id —
// the half-started container stays visible in the listing for inspection) ·
// 504 deploy timed out.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var spec docker.DeploySpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body: " + err.Error()})
		return
	}
	if err := spec.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), deployTimeout)
	defer cancel()
	res, err := s.docker.Deploy(ctx, &spec)
	if err != nil {
		var ae *docker.APIError
		var pe *docker.PullError
		switch {
		case errors.Is(err, docker.ErrNameConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.As(err, &pe):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": pe.Error()})
		case errors.Is(err, context.DeadlineExceeded):
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "deploy timed out (15 min)"})
		case errors.As(err, &ae) && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusNotFound):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": ae.Message})
		case errors.As(err, &ae):
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		default:
			// mkdir failure etc. — operator-actionable, not an engine problem
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "res": res, "time": now()})
}
