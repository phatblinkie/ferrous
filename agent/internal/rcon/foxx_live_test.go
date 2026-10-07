package rcon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLiveFoxxReadOnly is a real-protocol proof against the production Rust
// server (foxxservers.com) — the ONLY way to validate our hand-rolled
// websocket + WebRCON dialect against the genuine article before shipping.
//
// Gated behind FERROUS_LIVE_FOXX=1 and strictly READ-ONLY: status,
// serverinfo, playerlist verbose. No save/kick/ban/say/restart — ever.
func TestLiveFoxxReadOnly(t *testing.T) {
	if os.Getenv("FERROUS_LIVE_FOXX") != "1" {
		t.Skip("set FERROUS_LIVE_FOXX=1 to run the read-only production protocol proof")
	}
	host, port, pw := loadFoxxRcon(t, "/etc/rustweb.json")
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	t.Logf("connecting (read-only) to %s", addr)

	hub := New("foxx-live", func(context.Context) (Config, error) {
		return Config{Addr: addr, Password: pw}, nil
	}, Options{KeepaliveEvery: time.Hour}) // test runs well under 25s: no keepalive needed
	defer hub.Close()
	waitConnected(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// 1. status — plain text, read-only
	status, err := hub.Send(ctx, "status", 10*time.Second)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(status, "players") {
		t.Fatalf("unexpected status reply: %.200s", status)
	}
	t.Logf("status ok (%d bytes): %.100s", len(status), strings.ReplaceAll(status, "\n", " | "))

	// 2. serverinfo — JSON, read-only
	raw, err := hub.Send(ctx, "serverinfo", 10*time.Second)
	if err != nil {
		t.Fatalf("serverinfo: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("serverinfo is not JSON: %v (%.200s)", err, raw)
	}
	// real Rust capitalizes serverinfo keys ("Hostname", "Players", ...)
	if _, ok := info["Hostname"]; !ok {
		t.Fatalf("serverinfo missing Hostname: %.200s", raw)
	}
	t.Logf("serverinfo ok: hostname=%v players=%v", info["Hostname"], info["Players"])

	// 3. playerlist verbose — read-only; on a busy server this is a big
	// payload (exercises 16/64-bit frame lengths + correlation)
	raw, err = hub.Send(ctx, "playerlist verbose", 15*time.Second)
	if err != nil {
		t.Fatalf("playerlist verbose: %v", err)
	}
	var players []map[string]any
	if err := json.Unmarshal([]byte(raw), &players); err != nil {
		t.Fatalf("playerlist not JSON: %v (%.200s)", err, raw)
	}
	t.Logf("playerlist verbose ok: %d players (%d bytes)", len(players), len(raw))

	// 4. correlation still clean after the large payload (and any pushes)
	status, err = hub.Send(ctx, "status", 10*time.Second)
	if err != nil || !strings.Contains(status, "players") {
		t.Fatalf("post-payload status correlation broken: %q %v", status, err)
	}

	if _, _, pushes := hub.Status(); pushes > 0 {
		t.Logf("server pushes arrived and were discarded: %d", pushes)
	}
	t.Log("live read-only protocol proof PASSED")
}

// loadFoxxRcon extracts the foxx target's rcon credentials from the
// multi-server config (structure: {"servers":[{"id","rcon":{host,port,password}}]}).
func loadFoxxRcon(t *testing.T, path string) (host string, port int, password string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("config %s unreadable: %v", path, err)
	}
	var cfg struct {
		Servers []struct {
			ID   string `json:"id"`
			Rcon struct {
				Host     string `json:"host"`
				Port     int    `json:"port"`
				Password string `json:"password"`
			} `json:"rcon"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, s := range cfg.Servers {
		if s.ID == "foxx" {
			if s.Rcon.Host == "" || s.Rcon.Port == 0 || s.Rcon.Password == "" {
				t.Fatalf("foxx entry incomplete in %s", path)
			}
			return s.Rcon.Host, s.Rcon.Port, s.Rcon.Password
		}
	}
	t.Skipf("no foxx entry in %s", path)
	return "", 0, ""
}
