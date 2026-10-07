package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ferrous/agent/internal/docker"
)

// handleLogs: GET /api/v1/servers/{id}/logs?tail=N&follow=0|1
//
// SSE stream of `docker logs -f` (wings-style console output): first event is
// `hello`, then `log` events, terminated by `end` (container stopped), an
// `error` event (stream failure), or nothing (client disconnected).
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server id"})
		return
	}
	tail := 100
	if t := r.URL.Query().Get("tail"); t != "" {
		if t == "all" {
			tail = -1
		} else {
			n, err := strconv.Atoi(t)
			if err != nil || n < 0 || n > 10000 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tail must be 0..10000 or 'all'"})
				return
			}
			tail = n
		}
	}
	follow := r.URL.Query().Get("follow") != "0" // default: keep streaming

	// gate: managed containers only — and 404/502 land BEFORE SSE headers so
	// the client still gets a clean JSON status.
	ictx, icancel := context.WithTimeout(r.Context(), 5*time.Second)
	d, err := s.docker.Inspect(ictx, id)
	icancel()
	if err != nil {
		writeDockerError(w, err)
		return
	}
	if d.Config.Labels["ferrous.managed"] != "true" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a ferrous-managed container"})
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	_ = sseWrite(w, fl, map[string]any{
		"kind": "hello", "server": strings.TrimPrefix(d.Name, "/"),
		"tail": tail, "follow": follow, "tty": d.Config.Tty, "time": now(),
	})

	th := newThrottler(200, time.Second, func(notice string) {
		_ = sseWrite(w, fl, map[string]any{"kind": "log", "t": now(), "text": notice, "throttle": true})
	})

	err = s.docker.StreamLogs(r.Context(), id, d.Config.Tty, tail, follow, func(ll docker.LogLine) error {
		if !th.allow(time.Now()) {
			return nil
		}
		return sseWrite(w, fl, map[string]any{
			"kind": "log", "t": ll.Time.Format(time.RFC3339Nano), "text": ll.Text,
		})
	})
	th.flush() // summarize drops before the terminal event

	if err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return // client disconnected — normal
		}
		_ = sseWrite(w, fl, map[string]any{"kind": "error", "error": err.Error()})
		return
	}
	_ = sseWrite(w, fl, map[string]any{"kind": "end", "time": now()})
}

// throttler is a fixed-window console rate limiter (wings-style): at most max
// lines per window. On strike it notices once, counts drops, and summarizes
// when the window rolls over (or on flush() at stream end).
type throttler struct {
	max      int
	window   time.Duration
	start    time.Time
	count    int
	dropped  int
	striking bool
	notice   func(string)
}

func newThrottler(max int, window time.Duration, notice func(string)) *throttler {
	return &throttler{max: max, window: window, start: time.Now(), notice: notice}
}

func (t *throttler) allow(now time.Time) bool {
	if now.Sub(t.start) >= t.window {
		t.rollover()
	}
	if t.count < t.max {
		t.count++
		return true
	}
	t.dropped++
	if !t.striking {
		t.striking = true
		t.notice("[ferrous] console output too fast — throttling…")
	}
	return false
}

func (t *throttler) rollover() {
	if t.dropped > 0 {
		t.notice(fmt.Sprintf("[ferrous] %d lines dropped (console throttle)", t.dropped))
	}
	t.start, t.count, t.dropped, t.striking = time.Now(), 0, 0, false
}

// flush emits a pending drop summary (a burst at stream end never reaches the
// next window rollover on its own).
func (t *throttler) flush() {
	if t.dropped > 0 {
		t.notice(fmt.Sprintf("[ferrous] %d lines dropped (console throttle)", t.dropped))
		t.dropped = 0
	}
}

// sseWrite writes one `data: {json}\n\n` event and flushes immediately.
func sseWrite(w http.ResponseWriter, fl http.Flusher, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	fl.Flush()
	return nil
}
