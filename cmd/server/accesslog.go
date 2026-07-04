package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// accessLog wraps h with a structured per-request log: one slog record with
// method, path, status, byte count, duration, and client IP. Health and metrics
// probes log at debug so they don't drown the stream; 4xx logs at warn, 5xx at
// error. The recorder forwards Hijack and Flush so WebSocket upgrades (poder /
// agent tunnels, PTY) and streaming responses keep working through the wrapper.
func accessLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &accessRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)

		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		case r.URL.Path == "/health" || r.URL.Path == "/metrics":
			level = slog.LevelDebug
		}
		slog.Default().LogAttrs(context.Background(), level, "http",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.String("dur", time.Since(start).Round(time.Millisecond).String()),
			slog.String("remote", clientIP(r)),
		)
	})
}

// accessRecorder captures the status and byte count while preserving the
// optional interfaces the tunnel/stream paths rely on.
type accessRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *accessRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *accessRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Hijack keeps WebSocket upgrades (poder/agent tunnels, PTY) working through the
// wrapper; a successful hijack is recorded as a protocol switch (101).
func (r *accessRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("accesslog: underlying ResponseWriter is not a Hijacker")
	}
	if !r.wrote {
		r.status = http.StatusSwitchingProtocols
		r.wrote = true
	}
	return hj.Hijack()
}

// Flush keeps streaming responses (exec output) flushing through the wrapper.
func (r *accessRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// clientIP returns the first X-Forwarded-For hop (behind a load balancer) or the
// peer address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
