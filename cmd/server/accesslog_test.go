package main

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccessRecorder_CapturesStatusOnce(t *testing.T) {
	rec := &accessRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusNotFound)
	if rec.status != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.status)
	}
	rec.WriteHeader(http.StatusInternalServerError) // a second header write must not overwrite
	if rec.status != http.StatusNotFound {
		t.Errorf("status changed to %d after a second WriteHeader", rec.status)
	}
}

func TestAccessRecorder_DefaultsOKAndCountsBytes(t *testing.T) {
	rec := &accessRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	n, _ := rec.Write([]byte("hello"))
	if rec.status != http.StatusOK {
		t.Errorf("status=%d want 200 (no explicit WriteHeader)", rec.status)
	}
	if rec.bytes != int64(n) || rec.bytes != 5 {
		t.Errorf("bytes=%d want 5", rec.bytes)
	}
}

// hijackableRW stands in for the server's real connection, which WebSocket
// upgrades (poder/agent tunnels, PTY) hijack.
type hijackableRW struct {
	http.ResponseWriter
	conn net.Conn
}

func (h hijackableRW) Hijack() (net.Conn, *bufio.ReadWriter, error) { return h.conn, nil, nil }

// TestAccessRecorder_PreservesHijacker is the WS-safety guarantee: the wrapper
// must delegate Hijack so tunnel upgrades still work, and record it as 101.
func TestAccessRecorder_PreservesHijacker(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	rec := &accessRecorder{ResponseWriter: hijackableRW{httptest.NewRecorder(), c1}, status: http.StatusOK}
	conn, _, err := rec.Hijack()
	if err != nil {
		t.Fatalf("Hijack through recorder failed: %v", err)
	}
	if conn != c1 {
		t.Error("Hijack did not return the underlying connection")
	}
	if rec.status != http.StatusSwitchingProtocols {
		t.Errorf("status=%d want 101 after hijack", rec.status)
	}
}

func TestAccessRecorder_NonHijackerErrors(t *testing.T) {
	// httptest.ResponseRecorder is not an http.Hijacker.
	rec := &accessRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if _, _, err := rec.Hijack(); err == nil {
		t.Error("expected an error hijacking a non-Hijacker ResponseWriter")
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct{ name, xff, remote, want string }{
		{"xff single", "203.0.113.7", "10.0.0.1:5000", "203.0.113.7"},
		{"xff first hop", "203.0.113.7, 10.0.0.9", "10.0.0.1:5000", "203.0.113.7"},
		{"no xff", "", "10.0.0.1:5000", "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := clientIP(r); got != tt.want {
				t.Errorf("clientIP=%q want %q", got, tt.want)
			}
		})
	}
}
