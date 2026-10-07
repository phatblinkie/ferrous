// Command ferrous-agent is the remote-host daemon of the ferrous control plane.
//
// It listens for authenticated requests from the central panel and proxies a
// narrow, safe API over the host's Docker daemon: discovery, power, logs,
// stats, files (scoped per server), and an agent-side WebRCON hub.
//
// Design rules (phase 1 onward):
//   - stdlib only — single static binary, zero runtime dependencies
//   - auth on every endpoint, constant-time token compare
//   - docker trouble is reported (502 + friendly error), never a crash
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ferrous/agent/internal/api"
	"ferrous/agent/internal/docker"
)

// version is injected at build time: -ldflags "-X main.version=x.y.z"
var version = "dev"

func main() {
	listen := flag.String("listen", envOr("FERROUS_LISTEN", "127.0.0.1:8710"), "listen address (host:port)")
	token := flag.String("token", os.Getenv("FERROUS_TOKEN"), "API bearer token (generated when empty)")
	sock := flag.String("docker", envOr("FERROUS_DOCKER_SOCKET", "/var/run/docker.sock"), "docker unix socket")
	tlsCert := flag.String("tls-cert", os.Getenv("FERROUS_TLS_CERT"), "TLS certificate (with --tls-key enables HTTPS)")
	tlsKey := flag.String("tls-key", os.Getenv("FERROUS_TLS_KEY"), "TLS private key")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ferrous-agent", version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tok, generated := *token, false
	if tok == "" {
		tok, _ = genToken()
		generated = true
	}

	if _, err := os.Stat(*sock); err != nil {
		// degrade, don't die: /system and /servers will answer 502 until docker appears
		log.Warn("docker socket not found", "path", *sock, "err", err)
	}

	dc := docker.New(*sock)
	apiSrv := api.New(dc, tok, version, log)
	handler := apiSrv.Handler()
	hs := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shCtx)
		apiSrv.Close() // rcon hubs: stop keepalives and pending sends
	}()

	tlsOn := *tlsCert != "" && *tlsKey != ""
	log.Info("ferrous-agent starting",
		"addr", *listen, "tls", tlsOn, "version", version, "docker_socket", *sock)
	if generated {
		// first-boot UX: print the generated token once so it can be pasted into the panel
		log.Warn("generated API token — copy it into the panel now", "token", tok)
	}

	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Second)
	if v, err := dc.Version(pctx); err != nil {
		log.Warn("docker not reachable — system/servers endpoints will return 502 until it is", "err", err)
	} else {
		log.Info("docker connected", "version", v.Version, "api", v.APIVersion)
	}
	pcancel()

	var err error
	if tlsOn {
		err = hs.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = hs.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen failed", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func genToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
