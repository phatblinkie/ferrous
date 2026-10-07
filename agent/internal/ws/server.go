package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-5AB0DC85B11F"

// Accept performs the server-side handshake on an established connection
// (tests and the fakectorn fixture). Path must be /{password}; a wrong
// password gets a 401 and the connection is closed.
//
// timeout bounds the handshake itself; after success the deadline is cleared.
func Accept(nc net.Conn, password string, timeout time.Duration) (*Conn, error) {
	if err := nc.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	br := bufio.NewReader(nc)
	reject := func(code, msg string) error {
		_, _ = io.WriteString(nc, "HTTP/1.1 "+code+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		nc.Close()
		return fmt.Errorf("ws accept: %s", msg)
	}

	reqLine, err := br.ReadString('\n')
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ws accept read: %w", err)
	}
	parts := strings.Fields(reqLine)
	if len(parts) < 3 || parts[0] != "GET" {
		return nil, reject("400 Bad Request", "not a GET request")
	}
	key := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("ws accept read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Sec-WebSocket-Key") {
			key = strings.TrimSpace(v)
		}
	}
	if parts[1] != "/"+password {
		return nil, reject("401 Unauthorized", "bad password")
	}
	if key == "" {
		return nil, reject("400 Bad Request", "missing Sec-WebSocket-Key")
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := io.WriteString(nc, resp); err != nil {
		nc.Close()
		return nil, fmt.Errorf("ws accept write: %w", err)
	}
	if err := nc.SetDeadline(time.Time{}); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{c: nc, br: br, mask: false}, nil
}
