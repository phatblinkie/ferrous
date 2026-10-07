// Package rcon holds one persistent WebRCON connection per game server —
// a Go port of our proven python RconHub:
//
//   - supervisor goroutine: dial → recv loop → 8s backoff → re-dial. The
//     address/password are re-resolved every attempt (container IPs move on
//     restart; a cached addr would break exactly when reconnects matter).
//   - correlated replies: Identifier n ↔ pending[n]; Identifier 0 (server
//     pushes) is discarded — docker logs already carries console output.
//   - keepalive: a read-only `status` every 25s exercises the full send/recv
//     path (the only traffic we ever sustain toward a production server).
//   - Send distinguishes ErrNotConnected / ErrTimeout / other so the HTTP
//     layer can answer 503 / 504 / 503 — the panel's degradation UX depends
//     on that split (inventory: timeout → 404 "needs InvDump", lost → 503).
package rcon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"ferrous/agent/internal/ws"
)

// Config is a resolved WebRCON endpoint.
type Config struct {
	Addr     string // host:port (container IP:port)
	Password string
}

// Resolver re-derives the endpoint on every (re)connect attempt.
type Resolver func(ctx context.Context) (Config, error)

var (
	// ErrNotConnected: hub is down (wrapped with the reason when known).
	ErrNotConnected = errors.New("rcon not connected")
	// ErrTimeout: the command was sent but no reply arrived in time —
	// on a Rust server this also means "command does not exist" (vanilla
	// answers nothing to unknown rcon commands).
	ErrTimeout = errors.New("no RCON reply")
)

type reply struct {
	Message    string
	Stacktrace string
}

// Options tune the hub; zero values select production defaults.
type Options struct {
	KeepaliveEvery time.Duration // default 25s
	ReconnectDelay time.Duration // default 8s
	RecvIdle       time.Duration // read deadline per message; default 90s
	Log            *slog.Logger  // default: discarded
}

type Hub struct {
	label   string
	resolve Resolver
	opts    Options

	mu        sync.Mutex
	conn      *ws.Conn
	connected bool
	lastErr   string
	pending   map[int]chan reply
	cmdID     int
	shutdown  bool
	pushes    int64 // discarded Identifier:0 messages (diagnostics/tests)

	sendMu sync.Mutex // serialize frames: keepalive vs API callers

	firstDone chan struct{} // closed after the supervisor's first dial attempt
	firstOnce sync.Once

	done chan struct{}
	wg   sync.WaitGroup
}

func New(label string, resolve Resolver, opts Options) *Hub {
	if opts.KeepaliveEvery <= 0 {
		opts.KeepaliveEvery = 25 * time.Second
	}
	if opts.ReconnectDelay <= 0 {
		opts.ReconnectDelay = 8 * time.Second
	}
	if opts.RecvIdle <= 0 {
		opts.RecvIdle = 90 * time.Second
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &Hub{
		label:     label,
		resolve:   resolve,
		opts:      opts,
		pending:   make(map[int]chan reply),
		firstDone: make(chan struct{}),
		done:      make(chan struct{}),
	}
	h.wg.Add(2)
	go h.supervisor()
	go h.keepaliveLoop()
	return h
}

// --- connection lifecycle ---------------------------------------------------

func (h *Hub) supervisor() {
	defer h.wg.Done()
	state := "" // transition-only logging (avoid spamming per retry)
	for {
		select {
		case <-h.done:
			return
		default:
		}

		conn, err := h.dial()
		h.firstOnce.Do(func() { close(h.firstDone) }) // first command may await this
		if err != nil {
			h.setErr(err)
			if state != "down" {
				h.opts.Log.Info("rcon down", "hub", h.label, "err", err)
				state = "down"
			}
			if !h.sleep(h.opts.ReconnectDelay) {
				return
			}
			continue
		}
		if state != "up" {
			h.opts.Log.Info("rcon connected", "hub", h.label)
			state = "up"
		}
		err = h.recvLoop(conn)
		h.dropConn(conn, err)
		if state != "down" {
			h.opts.Log.Info("rcon connection lost", "hub", h.label, "err", err)
			state = "down"
		}
		if !h.sleep(h.opts.ReconnectDelay) {
			return
		}
	}
}

// dial resolves the endpoint (fresh every attempt — container IPs move) and
// completes the websocket handshake.
func (h *Hub) dial() (*ws.Conn, error) {
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	cfg, err := h.resolve(rctx)
	rcancel()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotConnected, err)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	conn, err := ws.Dial(dctx, cfg.Addr, cfg.Password)
	if err != nil {
		return nil, err
	}
	if !h.setConn(conn) {
		conn.Close()
		return nil, errors.New("hub closed")
	}
	return conn, nil
}

func (h *Hub) setConn(conn *ws.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shutdown {
		return false
	}
	h.conn = conn
	h.connected = true
	h.lastErr = ""
	return true
}

func (h *Hub) setErr(err error) {
	h.mu.Lock()
	h.lastErr = err.Error()
	h.mu.Unlock()
}

func (h *Hub) recvLoop(conn *ws.Conn) error {
	for {
		raw, err := conn.ReadMessage(time.Now().Add(h.opts.RecvIdle))
		if err != nil {
			return err
		}
		var m struct {
			Identifier int    `json:"Identifier"`
			Message    string `json:"Message"`
			Stacktrace string `json:"Stacktrace"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue // non-JSON chatter
		}
		if m.Identifier == 0 {
			h.mu.Lock()
			h.pushes++ // docker logs already carries console output
			h.mu.Unlock()
			continue
		}
		h.mu.Lock()
		ch := h.pending[m.Identifier]
		delete(h.pending, m.Identifier)
		h.mu.Unlock()
		if ch != nil {
			select {
			case ch <- reply{Message: m.Message, Stacktrace: m.Stacktrace}:
			default: // waiter already timed out
			}
		}
	}
}

// dropConn tears the connection down and fails every in-flight command so
// callers see an error immediately instead of waiting for their timeout.
func (h *Hub) dropConn(conn *ws.Conn, err error) {
	h.mu.Lock()
	if h.conn == conn {
		h.conn = nil
		h.connected = false
		if err != nil {
			h.lastErr = err.Error()
		}
	}
	pend := h.pending
	h.pending = make(map[int]chan reply)
	h.mu.Unlock()
	_ = conn.Close()
	for _, ch := range pend {
		select {
		case ch <- reply{Stacktrace: "connection lost"}:
		default:
		}
	}
}

// keepaliveLoop sends the read-only `status` command every KeepaliveEvery —
// the full send/recv path, nothing else (the production-safe keepalive).
func (h *Hub) keepaliveLoop() {
	defer h.wg.Done()
	t := time.NewTicker(h.opts.KeepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = h.Send(ctx, "status", 10*time.Second)
			cancel()
		}
	}
}

func (h *Hub) sleep(d time.Duration) bool { // false → shutdown
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.done:
		return false
	case <-t.C:
		return true
	}
}

// Close stops the hub (idempotent). Safe to call from a request handler.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.shutdown {
		h.mu.Unlock()
		return
	}
	h.shutdown = true
	conn := h.conn
	h.mu.Unlock()
	close(h.done)
	if conn != nil {
		_ = conn.Close() // unblock recvLoop immediately
	}
	h.wg.Wait()
}

// Status reports connection state for the GET /rcon endpoint.
func (h *Hub) Status() (connected bool, lastErr string, pushes int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected, h.lastErr, h.pushes
}

// --- commands ---------------------------------------------------------------

// Send runs cmd and waits for its correlated reply. Errors:
// ErrNotConnected (down), ErrTimeout (sent, no reply — vanilla's answer to
// unknown commands), anything else (server-side stacktrace / conn lost).
//
// The first command after hub creation waits (bounded by its own timeout) for
// the supervisor's first dial attempt — lazy hubs would otherwise always fail
// the first command with a spurious "not connected".
func (h *Hub) Send(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	if err := h.awaitConnected(ctx, timeout); err != nil {
		return "", err
	}

	h.mu.Lock()
	if !h.connected || h.conn == nil {
		err := h.notConnectedLocked()
		h.mu.Unlock()
		return "", err
	}
	h.cmdID++
	id := h.cmdID
	ch := make(chan reply, 1)
	h.pending[id] = ch
	conn := h.conn
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()

	payload, err := json.Marshal(map[string]any{
		"Identifier": id, "Message": cmd, "Name": "WebRcon",
	})
	if err != nil {
		return "", err
	}
	h.sendMu.Lock()
	werr := conn.WriteMessage(payload)
	h.sendMu.Unlock()
	if werr != nil {
		return "", fmt.Errorf("rcon send failed: %w", werr)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case rp := <-ch:
		if rp.Stacktrace != "" {
			return "", fmt.Errorf("rcon error: %s", rp.Stacktrace)
		}
		return rp.Message, nil
	case <-timer.C:
		return "", fmt.Errorf("%w for: %s", ErrTimeout, cmd)
	case <-ctx.Done():
		return "", ctx.Err()
	case <-h.done:
		return "", ErrNotConnected
	}
}

// awaitConnected is nil when the hub is (now) connected. A hub that has never
// attempted a dial waits for the first outcome; a previously-connected hub
// that is mid-reconnect fails immediately (panel polling will retry) — that
// matches the proven panel's behavior for a down server.
func (h *Hub) awaitConnected(ctx context.Context, timeout time.Duration) error {
	h.mu.Lock()
	if h.connected && h.conn != nil {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-h.firstDone:
	case <-timer.C:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.notConnectedLocked()
	case <-ctx.Done():
		return ctx.Err()
	case <-h.done:
		return ErrNotConnected
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connected && h.conn != nil {
		return nil
	}
	return h.notConnectedLocked()
}

// notConnectedLocked builds ErrNotConnected with the recorded reason (caller
// holds h.mu).
func (h *Hub) notConnectedLocked() error {
	if h.lastErr != "" {
		return fmt.Errorf("%w (%s)", ErrNotConnected, h.lastErr)
	}
	return ErrNotConnected
}
