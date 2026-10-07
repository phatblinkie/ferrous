package rcon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"ferrous/agent/internal/fakercon"
)

// fakeHub boots a fakercon server and a hub pointed at it.
func fakeHub(t *testing.T, opts fakercon.Options) (*fakercon.Server, *Hub) {
	t.Helper()
	if opts.Password == "" {
		opts.Password = "testpw"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := fakercon.New(ln, opts)
	t.Cleanup(func() { fake.Close() })

	addr := ln.Addr().String()
	hub := New("test", func(context.Context) (Config, error) {
		return Config{Addr: addr, Password: opts.Password}, nil
	}, Options{
		KeepaliveEvery: time.Hour, // tests control the traffic
		ReconnectDelay: 100 * time.Millisecond,
	})
	t.Cleanup(hub.Close)
	waitConnected(t, hub)
	return fake, hub
}

func waitConnected(t *testing.T, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _, _ := h.Status(); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, last, _ := h.Status()
	t.Fatalf("hub never connected (last error: %s)", last)
}

func TestSendCorrelatesConcurrentCommands(t *testing.T) {
	_, hub := fakeHub(t, fakercon.Options{ReplyDelay: 15 * time.Millisecond})
	var wg sync.WaitGroup
	errs := make(chan error, 8*5)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				cmd := fmt.Sprintf("say g%d-i%d", g, i)
				out, err := hub.Send(context.Background(), cmd, 5*time.Second)
				if err != nil {
					errs <- err
					return
				}
				if want := "Say: " + strings.TrimPrefix(cmd, "say "); out != want {
					errs <- fmt.Errorf("correlation mixup: sent %q got %q", want, out)
					return
				}
				errs <- nil
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnknownCommandTimesOut(t *testing.T) {
	// Rust (and the fake) stay silent on unknown commands.
	_, hub := fakeHub(t, fakercon.Options{})
	t0 := time.Now()
	_, err := hub.Send(context.Background(), "definitely.unknown.cmd", 300*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if d := time.Since(t0); d < 250*time.Millisecond {
		t.Fatalf("returned too early: %v", d)
	}
	if !strings.Contains(err.Error(), "definitely.unknown.cmd") {
		t.Fatalf("timeout error should name the command: %v", err)
	}
}

func TestServerStacktraceSurfaces(t *testing.T) {
	_, hub := fakeHub(t, fakercon.Options{})
	_, err := hub.Send(context.Background(), "fail", 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "simulated rcon error") {
		t.Fatalf("want stacktrace error, got %v", err)
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrNotConnected) {
		t.Fatalf("stacktrace must not masquerade as timeout/disconnect: %v", err)
	}
}

func TestNotConnectedWhenResolverFails(t *testing.T) {
	hub := New("broken", func(context.Context) (Config, error) {
		return Config{}, errors.New("container has no IP (not running?)")
	}, Options{ReconnectDelay: 50 * time.Millisecond})
	defer hub.Close()
	// wait for the supervisor's first resolve attempt to record its reason
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, last, _ := hub.Status(); last != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err := hub.Send(context.Background(), "status", time.Second)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
	if !strings.Contains(err.Error(), "no IP") {
		t.Fatalf("reason must be included: %v", err)
	}
}

func TestReconnectAfterConnectionDrop(t *testing.T) {
	fake, hub := fakeHub(t, fakercon.Options{})
	out, err := hub.Send(context.Background(), "status", 3*time.Second)
	if err != nil || !strings.Contains(out, "players") {
		t.Fatalf("first send: %q %v", out, err)
	}
	fake.DropConns() // simulate rcon restart: sockets die, listener lives
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _, _ := hub.Status(); !ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitConnected(t, hub) // supervisor re-dials (same listener, fresh socket)
	out, err = hub.Send(context.Background(), "stats", 3*time.Second)
	if err != nil || !strings.Contains(out, "cpu") {
		t.Fatalf("send after reconnect: %q %v", out, err)
	}
}

func TestInFlightCommandFailsFastOnDrop(t *testing.T) {
	// ReplyDelay keeps the reply in flight when we drop the sockets at 50ms.
	fake, hub := fakeHub(t, fakercon.Options{ReplyDelay: 400 * time.Millisecond})
	// fire a command the fake will delay past the drop, then drop mid-flight
	errCh := make(chan error, 1)
	go func() {
		_, err := hub.Send(context.Background(), "say slow", 10*time.Second)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond) // command is in flight
	fake.DropConns()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("in-flight command should fail when the connection dies")
		}
		if errors.Is(err, ErrTimeout) {
			t.Fatalf("should fail via connection-lost, not timeout: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight command hung — pending map was not failed on drop")
	}
}

func TestInvDumpVanillaSilenceVersusPlugin(t *testing.T) {
	// vanilla: invdump.get gets no reply → ErrTimeout (panel maps to 404)
	_, vanilla := fakeHub(t, fakercon.Options{InvDump: false})
	_, err := vanilla.Send(context.Background(), "invdump.get 76561198000000001", 250*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("vanilla must time out, got %v", err)
	}
	// plugin present: JSON reply
	_, modded := fakeHub(t, fakercon.Options{InvDump: true})
	out, err := modded.Send(context.Background(), "invdump.get 76561198000000001", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"belt"`) || !strings.Contains(out, "Assault Rifle") {
		t.Fatalf("unexpected invdump payload: %.120s", out)
	}
}

func TestPushesDiscardedWithoutBreakingCorrelation(t *testing.T) {
	fake, hub := fakeHub(t, fakercon.Options{PushEvery: 15 * time.Millisecond})
	for i := 0; i < 5; i++ {
		out, err := hub.Send(context.Background(), "serverinfo", 3*time.Second)
		if err != nil || !strings.Contains(out, "ferrous fake") {
			t.Fatalf("send %d: %q %v", i, out, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	var pushes int64
	for time.Now().Before(deadline) {
		if _, _, pushes = hub.Status(); pushes > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pushes == 0 {
		t.Fatal("expected Identifier:0 pushes to arrive (and be discarded)")
	}
	if fake.Cmds() < 5 {
		t.Fatalf("commands reached the server: %d", fake.Cmds())
	}
}

func TestKeepaliveSendsReadOnlyStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := fakercon.New(ln, fakercon.Options{Password: "testpw"})
	defer fake.Close()

	hub := New("ka", func(context.Context) (Config, error) {
		return Config{Addr: ln.Addr().String(), Password: "testpw"}, nil
	}, Options{KeepaliveEvery: 60 * time.Millisecond, ReconnectDelay: 50 * time.Millisecond})
	defer hub.Close()
	waitConnected(t, hub)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fake.Cmds() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fake.Cmds() < 1 {
		t.Fatal("keepalive never fired")
	}
	if last := fake.LastCmd(); last != "status" {
		t.Fatalf("keepalive must be the read-only status command, got %q", last)
	}
}
