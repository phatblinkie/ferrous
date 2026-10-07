package api

import (
	"context"
	"net/http"
	"time"
)

// handleStats: GET /api/v1/servers/{id}/stats — one-shot docker stats sample
// (cpu %, memory, network, pids).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// gate: managed containers only
	d, err := s.docker.Inspect(ctx, id)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	if d.Config.Labels["ferrous.managed"] != "true" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a ferrous-managed container"})
		return
	}

	st, err := s.docker.Stats(ctx, id)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "state": d.State.Status, "stats": st, "time": now(),
	})
}
