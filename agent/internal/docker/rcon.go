package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// ErrNoRCON: the container carries no RCON_PASSWORD — it is not a ferrous
// game server (the panel shows a friendly hint instead of polling forever).
var ErrNoRCON = errors.New("container has no RCON_PASSWORD env (rcon unavailable)")

// Rcon is a resolved WebRCON endpoint.
type Rcon struct {
	Addr     string // container IP:port — rcon ports are never published
	Password string
}

// RconConfig derives the WebRCON endpoint from a fresh inspect (the address
// must be re-derived every reconnect: container IPs move on restart).
func (c *Client) RconConfig(ctx context.Context, id string) (*Rcon, error) {
	d, err := c.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	return rconConfigFrom(d)
}

// rconConfigFrom is the pure derivation (unit-testable without a daemon):
//
//	password ← env RCON_PASSWORD        (required — server configures itself
//	                                      from the same env var)
//	port     ← label ferrous.rcon_port   ("" → 28016; label presence is also
//	                                      the capability flag in /servers)
//	address  ← container IP              (root IPAddress, else first network)
func rconConfigFrom(d *ContainerDetail) (*Rcon, error) {
	pw, ok := EnvValue(d.Config.Env, "RCON_PASSWORD")
	if !ok || pw == "" {
		return nil, ErrNoRCON
	}
	port := 28016
	if v, has := d.Config.Labels["ferrous.rcon_port"]; has && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("bad ferrous.rcon_port label %q (want 1-65535)", v)
		}
		port = n
	}
	ip := d.NetworkSettings.IPAddress
	if ip == "" {
		names := make([]string, 0, len(d.NetworkSettings.Networks))
		for n := range d.NetworkSettings.Networks {
			names = append(names, n)
		}
		sort.Strings(names) // deterministic pick
		for _, n := range names {
			if a := d.NetworkSettings.Networks[n].IPAddress; a != "" {
				ip = a
				break
			}
		}
	}
	if ip == "" {
		return nil, errors.New("container has no IP (not running?)")
	}
	return &Rcon{Addr: net.JoinHostPort(ip, strconv.Itoa(port)), Password: pw}, nil
}

// EnvValue scans docker's Env array ("KEY=value") for key.
func EnvValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if v, found := strings.CutPrefix(kv, prefix); found {
			return v, true
		}
	}
	return "", false
}
