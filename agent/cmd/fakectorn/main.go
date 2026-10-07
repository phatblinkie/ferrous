// Command fakectorn runs a fake Rust WebRCON server — a development and test
// fixture for the ferrous panel (players / console / inventory flows without
// a game server). It speaks the real protocol (path-auth handshake, JSON
// commands, Identifier:0 pushes) with canned replies from internal/fakercon.
//
// Usage:
//
//	fakectorn -listen 127.0.0.1:28016 -password secret -players 5 -invdump -push 3s
//
// -noinvdump (default) keeps the vanilla behavior: invdump.get is answered
// with silence, which exercises the panel's "needs uMod/InvDump" degradation.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ferrous/agent/internal/fakercon"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:28016", "listen address")
	password := flag.String("password", "ferrous", "rcon password (handshake path)")
	players := flag.Int("players", 3, "entries in 'playerlist verbose'")
	invdump := flag.Bool("invdump", false, "answer invdump.get (vanilla servers stay silent)")
	push := flag.Duration("push", 3*time.Second, "Identifier:0 console push interval (0 = off)")
	delay := flag.Duration("delay", 0, "artificial reply delay (interleaving tests)")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakectorn: listen: %v\n", err)
		os.Exit(1)
	}
	srv := fakercon.New(ln, fakercon.Options{
		Password:   *password,
		Players:    *players,
		InvDump:    *invdump,
		PushEvery:  *push,
		ReplyDelay: *delay,
	})
	log.Printf("fakectorn listening on %s (password %q, players %d, invdump %v, push %s)",
		ln.Addr(), *password, *players, *invdump, *push)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("fakectorn shutting down")
	_ = srv.Close()
}
