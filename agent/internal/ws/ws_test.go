package ws

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// wsPair establishes a real localhost TCP connection with client and server
// handshakes on each side (password must match).
func wsPair(t *testing.T, password string) (client, server *Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sch := make(chan *Conn, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			sch <- nil
			return
		}
		c, err := Accept(nc, password, 3*time.Second)
		sch <- c
	}()
	client, err = Dial(context.Background(), ln.Addr().String(), password)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	server = <-sch
	if server == nil {
		t.Fatal("server handshake failed")
	}
	t.Cleanup(func() { server.Close() })
	return client, server
}

func TestRoundTripAllLengthEncodings(t *testing.T) {
	c, s := wsPair(t, "pw")
	sizes := []int{5, 125, 126, 1000, 65535, 65536, 70000} // 7-bit, 16-bit, 64-bit edges
	for _, n := range sizes {
		payload := bytes.Repeat([]byte("x"), n)
		payload[0] = byte(n % 251)

		// client → server (masked)
		if err := c.WriteMessage(payload); err != nil {
			t.Fatalf("client write %d: %v", n, err)
		}
		got, err := s.ReadMessage(time.Now().Add(3 * time.Second))
		if err != nil {
			t.Fatalf("server read %d: %v", n, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("c→s payload mismatch at %d bytes (got %d)", n, len(got))
		}

		// server → client (unmasked)
		if err := s.WriteMessage(payload); err != nil {
			t.Fatalf("server write %d: %v", n, err)
		}
		got, err = c.ReadMessage(time.Now().Add(3 * time.Second))
		if err != nil {
			t.Fatalf("client read %d: %v", n, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("s→c payload mismatch at %d bytes (got %d)", n, len(got))
		}
	}
}

func TestClientFramesAreMasked(t *testing.T) {
	c, s := wsPair(t, "pw")
	if err := c.WriteMessage([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// read the raw header through the server's buffered reader: mask bit must
	// be set (RFC 6455 §5.3: client-to-server frames MUST be masked)
	b1, err := s.br.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.br.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	if b1&0x0F != 0x1 {
		t.Fatalf("opcode = 0x%x, want text 0x1", b1&0x0F)
	}
	if b2&0x80 == 0 {
		t.Fatal("client frame is NOT masked — RFC requires masking")
	}
}

func TestFragmentationReassembly(t *testing.T) {
	c, s := wsPair(t, "pw")
	// hand-craft: text frame without FIN + continuation with FIN (server→client, unmasked)
	raw := func(fin bool, opcode byte, payload []byte) {
		t.Helper()
		op := opcode
		if fin {
			op |= 0x80
		}
		var b []byte
		b = append(b, op, byte(len(payload))) // <126 for test payloads
		if _, err := s.c.Write(append(b, payload...)); err != nil {
			t.Fatal(err)
		}
	}
	raw(false, 0x1, []byte("frag-"))
	raw(true, 0x0, []byte("mented"))
	got, err := c.ReadMessage(time.Now().Add(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "frag-mented" {
		t.Fatalf("got %q", got)
	}
}

func TestPingGetsPong(t *testing.T) {
	c, s := wsPair(t, "pw")
	ping := []byte("keepalive-ping")
	// server → ping, then a text message
	if _, err := s.c.Write(append([]byte{0x89, byte(len(ping))}, ping...)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMessage([]byte("after-ping")); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadMessage(time.Now().Add(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after-ping" {
		t.Fatalf("got %q, ping should be transparent", got)
	}
	// the client must have answered with a matching pong (masked)
	f, err := s.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.opcode != 0xA {
		t.Fatalf("expected pong frame, got opcode 0x%x", f.opcode)
	}
	if !bytes.Equal(f.payload, ping) {
		t.Fatalf("pong payload %q != ping payload %q", f.payload, ping)
	}
}

func TestCloseFrameSurfaces(t *testing.T) {
	c, s := wsPair(t, "pw")
	if _, err := s.c.Write([]byte{0x88, 0x00}); err != nil { // close, no payload
		t.Fatal(err)
	}
	_, err := c.ReadMessage(time.Now().Add(3 * time.Second))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestReadTimeoutIsDeadlineExceeded(t *testing.T) {
	c, _ := wsPair(t, "pw")
	_, err := c.ReadMessage(time.Now().Add(-time.Second))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("want os.ErrDeadlineExceeded, got %v", err)
	}
}

func TestHandshakeRejectsWrongPassword(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		c, err := Accept(nc, "right-pw", 3*time.Second)
		if err == nil {
			c.Close()
		} // else: rejected (401 written by Accept)
	}()
	_, err = Dial(context.Background(), ln.Addr().String(), "wrong-pw")
	if err == nil {
		t.Fatal("dial with wrong password should fail")
	}
}

func TestConcurrentWritersDoNotInterleave(t *testing.T) {
	c, s := wsPair(t, "pw")
	// 8 goroutines × 50 messages: frames must never tear (write lock)
	done := make(chan error, 8)
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 50; i++ {
				if err := c.WriteMessage([]byte("msg")); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(g)
	}
	for g := 0; g < 8; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 400; i++ {
		if _, err := s.readFrame(); err != nil {
			t.Fatalf("torn frame at %d: %v", i, err)
		}
	}
}
