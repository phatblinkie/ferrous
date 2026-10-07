package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Dial performs the WebRCON client handshake. Rust authenticates the upgrade
// by PATH: GET /{password} (not the historical /websocket/{password}) — same
// as our proven python client. Handshake validation is intentionally lenient:
// the status line must be 101, nothing else is checked.
//
// Handshake budget: 10s, or earlier if ctx carries a deadline.
func Dial(ctx context.Context, addr, password string) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ws dial %s: %w", addr, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := nc.SetDeadline(deadline); err != nil {
		nc.Close()
		return nil, err
	}

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		nc.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	req := "GET /" + password + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		nc.Close()
		return nil, fmt.Errorf("ws handshake write: %w", err)
	}

	br := bufio.NewReader(nc)
	status := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("ws handshake read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if status == "" {
			status = line
		}
		if line == "" { // blank line ends the header block
			break
		}
	}
	if !strings.HasPrefix(status, "HTTP/1.1 101") && !strings.HasPrefix(status, "HTTP/1.0 101") {
		nc.Close()
		return nil, fmt.Errorf("ws handshake rejected: %s", status)
	}
	if err := nc.SetDeadline(time.Time{}); err != nil { // clear: long-lived reads
		nc.Close()
		return nil, err
	}
	return &Conn{c: nc, br: br, mask: true}, nil
}
