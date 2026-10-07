package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const dockerTimeout = 5 * time.Second

// handlePing is liveness only — deliberately never touches docker.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"version":    s.version,
		"started_at": s.started.Format(time.RFC3339),
		"uptime_s":   int64(time.Since(s.started).Seconds()),
		"time":       now(),
	})
}

// handleSystem reports engine version + host resources. Docker trouble maps to
// a clean 502 the panel can render as a friendly host-level error.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dockerTimeout)
	defer cancel()

	v, err := s.docker.Version(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	info, err := s.docker.Info(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"docker": map[string]any{
			"version":     v.Version,
			"api_version": v.APIVersion,
			"go_version":  v.GoVersion,
			"os":          v.OS,
			"arch":        v.Arch,
		},
		"host": map[string]any{
			"name":         info.Name,
			"ncpu":         info.NCPU,
			"mem_total_mb": info.MemTotal / (1024 * 1024),
			"containers": map[string]int{
				"total":   info.Containers,
				"running": info.ContainersRunning,
				"stopped": info.ContainersStopped,
				"paused":  info.ContainersPaused,
			},
			"images":         info.Images,
			"storage_driver": info.Driver,
		},
		"time": now(),
	})
}

// handleServers lists containers: managed by default (label ferrous.managed=true),
// everything with ?all=1. Each entry carries a "managed" flag so the panel can
// grey out / offer adoption for foreign containers.
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	filterName := "managed"
	label := "ferrous.managed=true"
	if all {
		filterName, label = "all", ""
	}

	ctx, cancel := context.WithTimeout(r.Context(), dockerTimeout)
	defer cancel()
	list, err := s.docker.ListContainers(ctx, true, label)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.pruneHubs(list) // containers that vanished take their hub with them

	servers := make([]map[string]any, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		labels := c.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		ports := make([]map[string]any, 0, len(c.Ports))
		for _, p := range c.Ports {
			pm := map[string]any{"private": p.PrivatePort, "proto": p.Type}
			if p.PublicPort != 0 {
				pm["public"] = p.PublicPort
				pm["ip"] = p.IP
			}
			ports = append(ports, pm)
		}
		_, rconCapable := labels["ferrous.rcon_port"] // capability flag (list has no env)
		servers = append(servers, map[string]any{
			"id":       c.Id,
			"short_id": shortID(c.Id),
			"name":     name,
			"image":    c.Image,
			"state":    c.State,
			"status":   c.Status,
			"created":  time.Unix(c.Created, 0).UTC().Format(time.RFC3339),
			"managed":  labels["ferrous.managed"] == "true",
			"rcon":     rconCapable,
			"labels":   labels,
			"ports":    ports,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": servers,
		"count":   len(servers),
		"filter":  filterName,
		"time":    now(),
	})
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
