// Package fakercon is a fake Rust WebRCON server: real protocol, canned
// replies. It backs hub unit tests and the fakectorn container fixture so
// players / console / inventory flows can be exercised end-to-end without a
// game server. Reply semantics mirror Rust where they matter:
//   - unknown commands get NO reply (that's how invdump.get behaves on vanilla)
//   - Identifier:0 console pushes interleave with replies
package fakercon

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ferrous/agent/internal/ws"
)

type Options struct {
	Password   string        // handshake path must be /{password}
	Players    int           // playerlist verbose entries (default 3)
	InvDump    bool          // answer invdump.get; false = vanilla silence
	PushEvery  time.Duration // Identifier:0 pushes (0 = off)
	ReplyDelay time.Duration // artificial latency so replies interleave
}

type Server struct {
	ln   net.Listener
	opts Options
	wg   sync.WaitGroup

	mu    sync.Mutex
	conns map[*ws.Conn]struct{}
	done  chan struct{}

	cmds    atomic.Int64
	lastCmd atomic.Value // string
}

// New accepts connections on ln until Close.
func New(ln net.Listener, opts Options) *Server {
	if opts.Players <= 0 {
		opts.Players = 3
	}
	s := &Server{ln: ln, opts: opts, conns: map[*ws.Conn]struct{}{}, done: make(chan struct{})}
	s.lastCmd.Store("")
	s.wg.Add(1)
	go s.acceptLoop()
	return s
}

func (s *Server) Addr() net.Addr   { return s.ln.Addr() }
func (s *Server) Cmds() int64      { return s.cmds.Load() }
func (s *Server) LastCmd() string  { return s.lastCmd.Load().(string) }
func (s *Server) Password() string { return s.opts.Password }

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(nc)
		}()
	}
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	conn, err := ws.Accept(nc, s.opts.Password, 5*time.Second)
	if err != nil {
		return // wrong password / handshake timeout
	}
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		conn.Close()
	}()

	stop := make(chan struct{})
	defer close(stop)
	if s.opts.PushEvery > 0 {
		go s.pushLoop(conn, stop)
	}

	for {
		raw, err := conn.ReadMessage(time.Now().Add(60 * time.Second))
		if err != nil {
			return
		}
		var req struct {
			Identifier int    `json:"Identifier"`
			Message    string `json:"Message"`
		}
		if json.Unmarshal(raw, &req) != nil || req.Identifier == 0 {
			continue
		}
		s.cmds.Add(1)
		s.lastCmd.Store(req.Message)
		if s.opts.ReplyDelay > 0 {
			time.Sleep(s.opts.ReplyDelay)
		}
		out, stack, ok := s.dispatch(req.Message)
		if !ok {
			continue // silence, like the real server
		}
		resp, _ := json.Marshal(map[string]any{
			"Identifier": req.Identifier,
			"Message":    out,
			"Type":       "GenericResponse",
			"Stacktrace": stack,
		})
		if err := conn.WriteMessage(resp); err != nil {
			return
		}
	}
}

func (s *Server) pushLoop(conn *ws.Conn, stop chan struct{}) {
	t := time.NewTicker(s.opts.PushEvery)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			n++
			msg := fmt.Sprintf(`{"Identifier":0,"Message":"[Server] fake console push %d","Name":"WebRcon"}`, n)
			if err := conn.WriteMessage([]byte(msg)); err != nil {
				return
			}
		}
	}
}

// dispatch decides the reply. Second return is the Stacktrace (error reply);
// reply=false means stay silent like the real server (unknown commands, and
// invdump.get on vanilla).
func (s *Server) dispatch(cmd string) (string, string, bool) {
	switch {
	case cmd == "status":
		return fmt.Sprintf("hostname: ferrous fake\nversion : 2026.10.1 linux\n"+
			"entities: 12345 (4.0)\nplayers : %d (64)", s.opts.Players), "", true
	case cmd == "stats":
		return "cpu 0.4 (0.6 avg)\nmem 4096 (8192 max)", "", true
	case cmd == "serverinfo":
		b, _ := json.Marshal(map[string]any{
			"Hostname": "ferrous fake", "Map": "Procedural Map", "GameMode": "survival",
			"MaxPlayers": 64, "Players": s.opts.Players, "Queued": 0,
			"Port": 28016, "Secure": true,
		})
		return string(b), "", true
	case cmd == "playerlist verbose":
		b, _ := json.Marshal(s.playerList())
		return string(b), "", true
	case cmd == "playerlist":
		var names []string
		for i := 1; i <= s.opts.Players; i++ {
			names = append(names, fmt.Sprintf("fake-%d", i))
		}
		b, _ := json.Marshal(names)
		return string(b), "", true
	case cmd == "plugins":
		return "Loaded 1 plugins\n  InvDump 2.0.0", "", true
	case cmd == "server.save":
		return "Saved", "", true
	case cmd == "fail":
		return "", "simulated rcon error", true
	case strings.HasPrefix(cmd, "say "):
		return "Say: " + strings.TrimPrefix(cmd, "say "), "", true
	case strings.HasPrefix(cmd, "kick "):
		return "Kicking " + strings.TrimPrefix(cmd, "kick "), "", true
	case strings.HasPrefix(cmd, "ban "):
		return "Banning " + strings.TrimPrefix(cmd, "ban "), "", true
	case strings.HasPrefix(cmd, "find "):
		return "2 matches", "", true
	case strings.HasPrefix(cmd, "invdump.get"):
		if !s.opts.InvDump {
			return "", "", false // vanilla: the command does not exist → silence
		}
		return s.invDump(strings.TrimSpace(strings.TrimPrefix(cmd, "invdump.get"))), "", true
	default:
		return "", "", false // unknown command: silence (Rust behavior)
	}
}

type position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type player struct {
	DisplayName      string   `json:"DisplayName"`
	SteamID          string   `json:"SteamID"`
	OwnerSteamID     string   `json:"OwnerSteamID"`
	EntityId         int      `json:"EntityId"`
	Address          string   `json:"Address"`
	Ping             int      `json:"Ping"`
	Health           float64  `json:"Health"`
	Position         position `json:"Position"`
	ConnectedSeconds int      `json:"ConnectedSeconds"`
	TeamID           string   `json:"TeamID"`
	IsMuted          bool     `json:"IsMuted"`
	ViolationLevel   float64  `json:"ViolationLevel"`
	CurrentLevel     int      `json:"CurrentLevel"`
}

func (s *Server) playerList() []player {
	health := []float64{100, 45.5, 80, 12.5, 66} // varied bars for the UI
	out := make([]player, 0, s.opts.Players)
	for i := 1; i <= s.opts.Players; i++ {
		team := "0"
		if i%2 == 0 {
			team = fmt.Sprintf("%d", 100+i)
		}
		out = append(out, player{
			DisplayName:      fmt.Sprintf("fake-%d", i),
			SteamID:          fmt.Sprintf("7656119800000000%d", i), // 17 digits
			OwnerSteamID:     "0",
			EntityId:         1000 + i,
			Address:          fmt.Sprintf("192.0.2.10:%d", 50000+i),
			Ping:             15 + 7*i,
			Health:           health[(i-1)%len(health)],
			Position:         position{X: float64(100 * i), Y: 5.5, Z: float64(-50 * i)},
			ConnectedSeconds: 60 * i * 7,
			TeamID:           team,
			IsMuted:          i == 3,
		})
	}
	return out
}

type invItem struct {
	Name      string    `json:"name"`
	Shortname string    `json:"shortname"`
	Amount    int       `json:"amount"`
	Cond      int       `json:"cond"`
	Cat       string    `json:"cat,omitempty"`
	Contents  []invItem `json:"contents,omitempty"`
}

func (s *Server) invDump(sid string) string {
	inv := map[string]any{
		"belt": []invItem{
			{Name: "Rock", Shortname: "rock", Amount: 1, Cond: 100},
			{Name: "Wood", Shortname: "wood", Amount: 500, Cond: 100, Cat: "resource"},
			{Name: "Assault Rifle", Shortname: "rifle.ak", Amount: 1, Cond: 87, Cat: "weapon"},
			{Name: "5.56 Rifle Ammo", Shortname: "ammo.rifle", Amount: 128, Cond: 100, Cat: "ammo"},
		},
		"Wear": []invItem{
			{Name: "Hoodie", Shortname: "hoodie", Amount: 1, Cond: 63, Cat: "clothing"},
			{Name: "Metal Facemask", Shortname: "metal.facemask", Amount: 1, Cond: 41, Cat: "clothing"},
		},
		"main": []invItem{
			{Name: "Large Wooden Box", Shortname: "box.wooden.large", Amount: 1, Cond: 100, Cat: "deployable",
				Contents: []invItem{
					{Name: "Sulfur", Shortname: "sulfur", Amount: 2400, Cond: 100, Cat: "resource"},
					{Name: "Medical Syringe", Shortname: "syringe.medical", Amount: 4, Cond: 100, Cat: "medical"},
				}},
		},
		"calories":  410,
		"hydration": 72.5,
	}
	b, _ := json.Marshal(map[string]any{
		"sid": sid,
		"player": map[string]any{"belt": inv["belt"], "wear": inv["Wear"], "main": inv["main"],
			"calories": inv["calories"], "hydration": inv["hydration"]},
		"updated": time.Now().UTC().Format(time.RFC3339),
	})
	return string(b)
}

// DropConns closes active websocket connections (simulates an rcon restart
// while the listener survives) — used by hub reconnect tests.
func (s *Server) DropConns() {
	s.mu.Lock()
	conns := make([]*ws.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// Close stops accepting and tears down every connection.
func (s *Server) Close() error {
	close(s.done)
	err := s.ln.Close()
	s.DropConns() // unblock connection loops — otherwise wg.Wait hangs a full read deadline
	s.wg.Wait()
	return err
}
