// Copyright 2026 SandrPod Contributors
// Regression tests for bugs found in review: each one failed before its fix.
package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The handshake parser buffers more than the handshake; those bytes belong to
// the tunnel and must be re-attached before splicing.
func TestBufferedConn_ReplaysBytesReadPastTheHandshake(t *testing.T) {
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()

	br := bufio.NewReader(pr)
	go func() {
		_, _ = pw.Write([]byte("head"))
		_, _ = pw.Write([]byte("tail"))
	}()
	head := make([]byte, 4)
	if _, err := io.ReadFull(br, head); err != nil {
		t.Fatal(err)
	}
	// The reader has buffered "tail" (or will); the tunnel must see it.
	conn := newBufferedConn(pr, br)
	_ = pr.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("buffered bytes were lost: %v", err)
	}
	if string(got) != "tail" {
		t.Fatalf("got %q", got)
	}
}

// An inherited NO_PROXY must not survive into the sandbox.
func TestProxyEnv_EmptiesNoProxy(t *testing.T) {
	s, _ := startProxy(t, Policy{AllowedDomains: []string{"example.com"}})
	found := map[string]bool{}
	for _, kv := range s.ProxyEnv() {
		if kv == "NO_PROXY=" || kv == "no_proxy=" {
			found[kv] = true
		}
	}
	if len(found) != 2 {
		t.Fatalf("NO_PROXY/no_proxy must be emptied, got %v", s.ProxyEnv())
	}
}

// A truncated upstream body must abort the response, not deliver a well-formed
// short one.
func TestForward_TruncatedBodyAborts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
		// No chunk terminator: the handler returns while framing says more is
		// coming. net/http writes the final chunk, so this is not enough; the
		// proxy must not stitch a clean end onto a broken stream.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort"))
				conn.Close()
			}
			return
		}
	}))
	defer upstream.Close()

	compiled, _ := Parse(Policy{AllowedDomains: []string{"127.0.0.1:" + mustPort(t, upstream.URL)}})
	srv, err := Start(Options{Policy: compiled})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := proxyClient(t, srv)
	resp, err := client.Get(upstream.URL)
	if err != nil {
		// The proxy aborted the connection: the correct outcome, since the
		// upstream promised 100 bytes and sent 5.
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil && string(body) == "short" {
		t.Fatal("a truncated body was delivered as complete")
	}
	if err == nil && int64(len(body)) < resp.ContentLength {
		t.Fatalf("a short body was delivered without an error: %d of %d", len(body), resp.ContentLength)
	}
}

// The decider cache must not grow without bound.
func TestDecider_CacheIsBounded(t *testing.T) {
	compiled, _ := Parse(Policy{})
	var calls atomic.Int64
	srv, err := Start(Options{
		Policy:         compiled,
		DecideCacheTTL: time.Millisecond,
		Decide: func(ctx context.Context, host string, port int) (Verdict, error) {
			calls.Add(1)
			return Verdict{Allow: false, Code: CodeNotAllowed, Reason: "no"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	for i := 0; i < 3000; i++ {
		_ = srv.decide(context.Background(), "host"+itoa(i)+".example", 443)
	}
	srv.verdictMu.Lock()
	n := len(srv.verdicts)
	srv.verdictMu.Unlock()
	if n > 2048 {
		t.Fatalf("the verdict cache kept %d entries", n)
	}
}

// A wrong token is a refusal like any other: counted and audited.
func TestAuthFailure_IsRecorded(t *testing.T) {
	var events []Event
	s, _ := startProxyWithSink(t, Policy{AllowedDomains: []string{"example.com"}}, "/wrong", func(e Event) {
		events = append(events, e)
	})
	proxyURL, _ := url.Parse(s.ProxyURL())
	u := *proxyURL
	u.User = url.UserPassword("sandbox", "not-the-token")
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&u)}}
	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, denied := s.Stats(); denied == 0 {
		t.Error("the refusal was not counted")
	}
	var sawAuth bool
	for _, e := range events {
		if e.Code == CodeAuthRequired {
			sawAuth = true
		}
	}
	if !sawAuth {
		t.Errorf("the refusal was not audited: %+v", events)
	}
}

// A trailing-dot entry is a valid FQDN, not a dead line.
func TestPolicy_TrailingDotEntryMatches(t *testing.T) {
	compiled, err := Parse(Policy{AllowedDomains: []string{"example.com.", "*.npmjs.org."}})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if v := compiled.Decide("example.com", 443); !v.Allow {
		t.Errorf("example.com. did not match example.com: %+v", v)
	}
	if v := compiled.Decide("registry.npmjs.org", 443); !v.Allow {
		t.Errorf("*.npmjs.org. did not match registry.npmjs.org: %+v", v)
	}
}

// Headers the client marked hop-by-hop must not be forwarded.
func TestStripHopByHop_HonoursConnectionTokens(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Hop")
	h.Set("X-Hop", "secret")
	h.Set("Keep-Alive", "timeout=5")
	stripHopByHop(h)
	if h.Get("X-Hop") != "" {
		t.Error("a header named in Connection was forwarded")
	}
	if h.Get("Connection") != "" || h.Get("Keep-Alive") != "" {
		t.Error("standard hop-by-hop headers survived")
	}
}

func mustPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func startProxyWithSink(t *testing.T, p Policy, _ string, sink func(Event)) (*Server, *Compiled) {
	t.Helper()
	compiled, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Options{Policy: compiled, LogAllowed: true, Sink: sinkFunc(sink)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, compiled
}

var _ = strings.TrimSpace
