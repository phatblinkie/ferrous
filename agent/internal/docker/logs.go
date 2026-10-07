package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// LogLine is one demultiplexed, timestamp-stripped output line.
type LogLine struct {
	Stream string // "stdout" | "stderr"
	Time   time.Time
	Text   string
}

// StreamLogs tails (and optionally follows) container output, invoking fn per
// line. Blocks until the ctx is cancelled, the reader errors, or — with
// follow=true — the stream closes because the container stopped: that path
// returns nil so the caller can emit a clean end event.
//
// tty must come from Inspect: TTY containers emit a raw byte stream while
// non-TTY containers multiplex stdout/stderr in 8-byte framed packets
// (byte 0 = stream, bytes 4-7 = big-endian payload length).
func (c *Client) StreamLogs(ctx context.Context, id string, tty bool, tail int, follow bool, fn func(LogLine) error) error {
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("timestamps", "1") // RFC3339Nano prefix → real line times for the panel
	if tail >= 0 {
		q.Set("tail", strconv.Itoa(tail)) // tail < 0 = engine default (all)
	}
	if follow {
		q.Set("follow", "1")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/containers/"+url.PathEscape(id)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return apiErrorFrom(res)
	}
	return scanLogStream(res.Body, tty, fn)
}

// scanLogStream demultiplexes the engine log stream and invokes fn per line.
// Line buffers are local to the call so concurrent streams never share state.
func scanLogStream(r io.Reader, tty bool, fn func(LogLine) error) error {
	stdout, stderr := &lineBuf{}, &lineBuf{}

	feed := func(stream string, payload []byte) error {
		buf := stdout
		if stream == "stderr" {
			buf = stderr
		}
		var emitErr error
		buf.feed(payload, func(line []byte) {
			if emitErr != nil {
				return
			}
			ts, text := splitTimestamp(line)
			emitErr = fn(LogLine{Stream: stream, Time: ts, Text: text})
		})
		return emitErr
	}

	if tty {
		chunk := make([]byte, 32*1024)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				if ferr := feed("stdout", chunk[:n]); ferr != nil {
					return ferr
				}
			}
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
	}

	// multiplexed: 8-byte header + payload
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil // container exited — clean close
			}
			return err
		}
		length := binary.BigEndian.Uint32(hdr[4:8])
		if length == 0 {
			continue
		}
		if length > 16<<20 {
			return fmt.Errorf("docker log frame too large: %d bytes", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		if err := feed(stream, payload); err != nil {
			return err
		}
	}
}

// lineBuf accumulates bytes until a newline so logical lines may span frames.
// The slice handed to emit is only valid for the duration of the call.
type lineBuf struct{ b []byte }

func (l *lineBuf) feed(p []byte, emit func([]byte)) {
	l.b = append(l.b, p...)
	for {
		i := bytes.IndexByte(l.b, '\n')
		if i < 0 {
			return
		}
		emit(l.b[:i])
		l.b = l.b[i+1:]
	}
}

// splitTimestamp parses docker's "2026-10-07T05:40:01.123456789Z text" stamp.
func splitTimestamp(line []byte) (time.Time, string) {
	i := bytes.IndexByte(line, ' ')
	if i >= 0 {
		if t, err := time.Parse(time.RFC3339Nano, string(line[:i])); err == nil {
			return t.UTC(), trimCR(line[i+1:])
		}
	}
	return time.Now().UTC(), trimCR(line)
}

func trimCR(b []byte) string { return string(bytes.TrimRight(b, "\r")) }
