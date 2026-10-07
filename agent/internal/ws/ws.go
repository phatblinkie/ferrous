// Package ws is a minimal RFC 6455 WebSocket implementation — both roles —
// just enough to speak WebRCON to Rust servers (client) and to fake one in
// tests and fixtures (server). Stdlib only: the agent must stay a static,
// dependency-free binary.
//
// Scope decisions (deliberately narrow):
//   - text messages only (WebRCON is JSON over text frames)
//   - FIN + continuation reassembly, control frames handled inline
//     (ping → auto-pong, close → ErrClosed)
//   - client frames masked (RFC requires it), server frames unmasked
//   - handshake validation is lenient on Sec-WebSocket-Accept (matches our
//     proven python client — status line is checked, nothing else)
package ws

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// maxMessage caps reassembled messages (playerlist verbose on a big server is
// a few MB; 32MB is a generous ceiling against memory blowups).
const maxMessage = 32 << 20

// ErrClosed is a close frame from the peer, or EOF at a message boundary.
var ErrClosed = errors.New("websocket closed by peer")

// Conn is an established websocket. Safe for one concurrent reader plus any
// number of writers (writes are serialized internally).
type Conn struct {
	c    net.Conn
	br   *bufio.Reader
	mask bool // outgoing frames must be masked (client role)
	wmu  sync.Mutex
}

// WriteMessage sends payload as a single FIN text frame.
func (c *Conn) WriteMessage(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrame(0x1, payload)
}

// writeFrame writes one frame; caller must hold wmu.
// Header: [FIN|opcode] [MASK|7-bit-len] [ext len…] [mask key…] [payload…] —
// the mask bit shares byte 2 with the length (RFC 6455 §5.2).
func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	n := len(payload)
	var lenByte byte
	switch {
	case n < 126:
		lenByte = byte(n)
	case n < 65536:
		lenByte = 126
	default:
		lenByte = 127
	}
	if c.mask {
		lenByte |= 0x80
	}
	hdr := make([]byte, 0, 14+n)
	hdr = append(hdr, 0x80|opcode, lenByte)
	switch {
	case n < 126:
	case n < 65536:
		hdr = append(hdr, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, ext[:]...)
	}
	body := payload
	if c.mask {
		var maskKey [4]byte
		if _, err := rand.Read(maskKey[:]); err != nil {
			return fmt.Errorf("mask key: %w", err)
		}
		hdr = append(hdr, maskKey[:]...)
		body = make([]byte, n)
		for i, b := range payload {
			body[i] = b ^ maskKey[i%4]
		}
	}
	if _, err := c.c.Write(append(hdr, body...)); err != nil {
		return fmt.Errorf("ws write: %w", err)
	}
	return nil
}

// ReadMessage returns the next complete text message. Deadline applies to the
// whole message (first frame); idle peers surface os.ErrDeadlineExceeded —
// the hub turns that into a reconnect. Ping frames are answered with pong and
// skipped; fragmentation is reassembled transparently.
func (c *Conn) ReadMessage(deadline time.Time) ([]byte, error) {
	if err := c.c.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	var msg []byte
	started := false
	for {
		f, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch f.opcode {
		case 0x8: // close
			return nil, ErrClosed
		case 0x9: // ping → pong (RFC: pong must reply with the same payload)
			c.wmu.Lock()
			_ = c.writeFrame(0xA, f.payload)
			c.wmu.Unlock()
			continue
		case 0xA: // pong
			continue
		case 0x0: // continuation
			if !started {
				return nil, errors.New("ws: unexpected continuation frame")
			}
		case 0x1, 0x2: // text / binary (binary tolerated, WebRCON never sends it)
			if started {
				return nil, errors.New("ws: new message before continuation finished")
			}
			started = true
		default:
			return nil, fmt.Errorf("ws: unsupported opcode 0x%x", f.opcode)
		}
		msg = append(msg, f.payload...)
		if len(msg) > maxMessage {
			return nil, fmt.Errorf("ws: message exceeds %d bytes", maxMessage)
		}
		if f.fin && started {
			return msg, nil
		}
	}
}

type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

func (c *Conn) readFrame() (*frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return nil, wrapReadErr(err)
	}
	f := &frame{fin: h[0]&0x80 != 0, opcode: h[0] & 0x0F}
	masked := h[1]&0x80 != 0
	n := int64(h[1] & 0x7F)
	switch n {
	case 126:
		var e [2]byte
		if _, err := io.ReadFull(c.br, e[:]); err != nil {
			return nil, wrapReadErr(err)
		}
		n = int64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(c.br, e[:]); err != nil {
			return nil, wrapReadErr(err)
		}
		n = int64(binary.BigEndian.Uint64(e[:]))
	}
	if n > maxMessage {
		return nil, fmt.Errorf("ws: frame too large: %d bytes", n)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return nil, wrapReadErr(err)
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(c.br, f.payload); err != nil {
		return nil, wrapReadErr(err)
	}
	if masked {
		for i := range f.payload {
			f.payload[i] ^= maskKey[i%4]
		}
	}
	return f, nil
}

func wrapReadErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("ws: connection lost: %w", err)
	}
	return err // timeouts pass through untouched (os.ErrDeadlineExceeded)
}

// Close performs a best-effort graceful close (status 1000) then drops the
// transport. Safe on an already-broken connection.
func (c *Conn) Close() error {
	c.wmu.Lock()
	_ = c.writeFrame(0x8, []byte{0x03, 0xE8}) // 1000 = normal closure
	c.wmu.Unlock()
	return c.c.Close()
}

// SetReadDeadline is exported for callers that idle-wait between reads.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }
