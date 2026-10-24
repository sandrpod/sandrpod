// Copyright 2026 SandrPod
// End-to-end tests for the two proxy listeners: a real client, a real
// upstream, and the verdicts in between.

package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer accepts TCP connections and echoes each line back, prefixed so a
// test can tell its own bytes from the proxy's.
func echoServer(t *testing.T) (addr string, port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return ln.Addr().String(), a.Port, func() { _ = ln.Close() }
}

func startProxy(t *testing.T, p Policy) (*Server, *recordingSink) {
	t.Helper()
	c, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sink := &recordingSink{}
	s, err := Start(Options{Policy: c, Sink: sink})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, sink
}

type recordingSink struct{ events []Event }

func (r *recordingSink) RecordEgress(e Event) { r.events = append(r.events, e) }

func (r *recordingSink) last() (Event, bool) {
	if len(r.events) == 0 {
		return Event{}, false
	}
	return r.events[len(r.events)-1], true
}

// proxyClient builds an http.Client that goes through the proxy with its
// credentials, the way curl -x user:pass@host:port does.
func proxyClient(t *testing.T, s *Server) *http.Client {
	t.Helper()
	u, err := url.Parse(s.ProxyURL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(u)},
		Timeout:   5 * time.Second,
	}
}

func TestHTTP_AllowRefuseAndAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()

	upURL, _ := url.Parse(upstream.URL)
	_, upPort, _ := net.SplitHostPort(upURL.Host)
	var port int
	_, err := fmt.Sscanf(upPort, "%d", &port)
	if err != nil {
		t.Fatalf("upstream port: %v", err)
	}

	s, sink := startProxy(t, Policy{AllowedDomains: []string{"127.0.0.1:" + upPort}})
	client := proxyClient(t, s)

	// Allowed: an explicitly allow-listed IP literal.
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("allowed request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from upstream" {
		t.Fatalf("allowed request: status=%d body=%q", resp.StatusCode, body)
	}

	// Refused: a host nobody allow-listed.
	resp, err = client.Get("http://example.org/")
	if err != nil {
		t.Fatalf("denied request should still get a response: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied request: status=%d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Deny-Reason"); got != CodeNotAllowed {
		t.Errorf("X-Deny-Reason = %q, want %q", got, CodeNotAllowed)
	}
	if !strings.Contains(string(body), "not on the egress allow list") {
		t.Errorf("deny body should tell the model what to do: %q", body)
	}

	// The denial is on the record, with the destination and the code.
	ev, ok := sink.last()
	if !ok {
		t.Fatal("no event recorded for the denial")
	}
	if ev.Host != "example.org" || ev.Port != 80 || ev.Code != CodeNotAllowed || ev.Kind != CodeHTTP {
		t.Errorf("unexpected event: %+v", ev)
	}

	// No credentials: 407, and nothing is dialled.
	anon := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: s.HTTPAddr()})},
		Timeout:   5 * time.Second,
	}
	resp, err = anon.Get(upstream.URL)
	if err != nil {
		t.Fatalf("anonymous request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("anonymous request: status=%d, want 407", resp.StatusCode)
	}
}

// A name in the allow list that resolves to loopback is still refused: the
// allow list names a destination, the guard decides what that name may reach.
func TestHTTP_GuardRefusesNameThatResolvesToLoopback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, upPort, _ := net.SplitHostPort(upURL.Host)

	s, sink := startProxy(t, Policy{AllowedDomains: []string{"localhost:" + upPort}})
	client := proxyClient(t, s)

	resp, err := client.Get("http://localhost:" + upPort + "/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 from the address check", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Deny-Reason"); got != CodeDeniedAddress {
		t.Errorf("X-Deny-Reason = %q, want %q", got, CodeDeniedAddress)
	}
	ev, ok := sink.last()
	if !ok || !strings.Contains(ev.Reason, "loopback") {
		t.Errorf("event should name the address class: %+v", ev)
	}
}

func TestCONNECT_TunnelAndRefusal(t *testing.T) {
	addr, port, stop := echoServer(t)
	defer stop()

	s, sink := startProxy(t, Policy{AllowedDomains: []string{"127.0.0.1:" + fmt.Sprint(port)}})

	// Allowed tunnel: bytes go both ways untouched.
	conn := dialProxy(t, s, addr)
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if line != "ping\n" {
		t.Fatalf("tunnel carried %q", line)
	}

	// Refused tunnel: 403 before anything is dialled.
	conn2 := dialProxyRaw(t, s, "example.org:443")
	defer conn2.Close()
	head, err := bufio.NewReader(conn2).ReadString('\n')
	if err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if !strings.Contains(head, "403") {
		t.Fatalf("refusal status line = %q, want 403", head)
	}
	ev, _ := sink.last()
	if ev.Kind != CodeConnect || ev.Host != "example.org" || ev.Port != 443 {
		t.Errorf("unexpected connect event: %+v", ev)
	}
}

// dialProxy opens a CONNECT tunnel and returns the raw connection.
func dialProxy(t *testing.T, s *Server, target string) net.Conn {
	t.Helper()
	conn := dialProxyRaw(t, s, target)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != 200 {
		conn.Close()
		t.Fatalf("CONNECT status = %d", resp.StatusCode)
	}
	return conn
}

// dialProxyRaw sends CONNECT and returns the connection before the response
// is read, so a refusal test can inspect the status line.
func dialProxyRaw(t *testing.T, s *Server, target string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", s.HTTPAddr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("sandbox:" + s.Token()))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n",
		target, target, auth)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	return conn
}

func TestSOCKS_HandshakeAuthAndTunnel(t *testing.T) {
	addr, port, stop := echoServer(t)
	defer stop()

	s, sink := startProxy(t, Policy{AllowedDomains: []string{"127.0.0.1:" + fmt.Sprint(port)}})

	conn, err := net.Dial("tcp", s.SOCKSAddr())
	if err != nil {
		t.Fatalf("dial socks: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Greeting: version 5, one method (username/password).
	if _, err := conn.Write([]byte{5, 1, 2}); err != nil {
		t.Fatal(err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		t.Fatalf("method selection: %v", err)
	}
	if sel != [2]byte{5, 2} {
		t.Fatalf("method selection = %v, want [5 2]", sel)
	}

	// RFC 1929 auth with the proxy token.
	pass := []byte(s.Token())
	msg := append([]byte{1, byte(len("sandbox"))}, []byte("sandbox")...)
	msg = append(msg, byte(len(pass)))
	msg = append(msg, pass...)
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	var authReply [2]byte
	if _, err := io.ReadFull(conn, authReply[:]); err != nil {
		t.Fatalf("auth reply: %v", err)
	}
	if authReply != [2]byte{1, 0} {
		t.Fatalf("auth reply = %v, want success", authReply)
	}

	// CONNECT 127.0.0.1:port by IPv4.
	req := []byte{5, 1, 0, 1, 127, 0, 0, 1, byte(port >> 8), byte(port)}
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if reply[1] != 0 {
		t.Fatalf("connect reply code = %d, want 0", reply[1])
	}

	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if line != "hello\n" {
		t.Fatalf("tunnel carried %q", line)
	}
	_ = addr

	// A denied destination answers "not allowed by ruleset" (0x02).
	conn2, err := net.Dial("tcp", s.SOCKSAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	_ = conn2.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn2.Write([]byte{5, 1, 2})
	_, _ = io.ReadFull(conn2, sel[:])
	_, _ = conn2.Write(msg)
	_, _ = io.ReadFull(conn2, authReply[:])
	domain := []byte("example.org")
	denied := append([]byte{5, 1, 0, 3, byte(len(domain))}, domain...)
	denied = append(denied, 1, 187) // port 443
	_, _ = conn2.Write(denied)
	reply2 := make([]byte, 10)
	if _, err := io.ReadFull(conn2, reply2); err != nil {
		t.Fatalf("denied connect reply: %v", err)
	}
	if reply2[1] != socksRepNotAllowed {
		t.Fatalf("denied reply code = %#x, want %#x", reply2[1], socksRepNotAllowed)
	}
	ev, ok := sink.last()
	if !ok || ev.Kind != CodeSOCKS || ev.Host != "example.org" || ev.Port != 443 {
		t.Errorf("unexpected socks event: %+v", ev)
	}
}

// A client that offers only "no auth" must be refused: the token is what
// keeps other local processes out of the sandbox's allow list.
func TestSOCKS_RefusesNoAuth(t *testing.T) {
	s, _ := startProxy(t, Policy{AllowedDomains: []string{"example.com"}})
	conn, err := net.Dial("tcp", s.SOCKSAddr())
	if err != nil {
		t.Fatalf("dial socks: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		t.Fatalf("method selection: %v", err)
	}
	if sel != [2]byte{5, 0xFF} {
		t.Fatalf("selection = %v, want [5 255] (no acceptable method)", sel)
	}
}

func TestProxyEnv_Shape(t *testing.T) {
	s, _ := startProxy(t, Policy{AllowedDomains: []string{"example.com"}})
	env := strings.Join(s.ProxyEnv(), "\n")
	for _, want := range []string{"HTTP_PROXY=http://sandbox:", "https_proxy=http://sandbox:",
		"ALL_PROXY=socks5://sandbox:", "NODE_USE_ENV_PROXY=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("ProxyEnv missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "NO_PROXY") {
		t.Error("NO_PROXY must not be set: it is a bypass list")
	}
	if !strings.Contains(s.ProxyURL(), "127.0.0.1:") {
		t.Errorf("proxy must bind loopback, got %q", s.ProxyURL())
	}
}

func TestStats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, upPort, _ := net.SplitHostPort(upURL.Host)

	s, _ := startProxy(t, Policy{AllowedDomains: []string{"127.0.0.1:" + upPort}})
	client := proxyClient(t, s)

	if resp, err := client.Get(upstream.URL); err == nil {
		resp.Body.Close()
	}
	if resp, err := client.Get("http://example.org/"); err == nil {
		resp.Body.Close()
	}

	allowed, denied := s.Stats()
	if allowed != 1 || denied != 1 {
		t.Errorf("stats = allowed %d denied %d, want 1/1", allowed, denied)
	}
}

// The transport is what makes a pooled connection carry a verdict, so a
// direct call to its dial must refuse too (defence in depth against a future
// caller that skips handleForward).
func TestTransportDial_RechecksPolicy(t *testing.T) {
	s, _ := startProxy(t, Policy{AllowedDomains: []string{"example.com"}})
	_, err := s.transport.DialContext(context.Background(), "tcp", "example.org:80")
	if err == nil {
		t.Fatal("transport dial should refuse a host outside the policy")
	}
	if _, ok := asDenyError(err); !ok {
		t.Fatalf("expected a DenyError, got %v", err)
	}
}

// A policy change must land on a running server: same listeners, same token,
// new verdicts on the next connection.
func TestUpdatePolicy_TakesEffectWithoutRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, upPort, _ := net.SplitHostPort(upURL.Host)

	s, _ := startProxy(t, Policy{AllowedDomains: []string{"example.com"}})
	client := proxyClient(t, s)

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("before the update: status=%d, want 403", resp.StatusCode)
	}

	updated, err := Parse(Policy{AllowedDomains: []string{"127.0.0.1:" + upPort}})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := s.UpdatePolicy(updated); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	if s.Policy() != updated {
		t.Error("Policy() should report the policy just installed")
	}

	resp, err = client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request after update: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after the update: status=%d, want 200", resp.StatusCode)
	}

	// Tightening works the same way: the next request is refused again.
	tightened, err := Parse(Policy{AllowedDomains: []string{}})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := s.UpdatePolicy(tightened); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	resp, err = client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request after tightening: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("after tightening: status=%d, want 403", resp.StatusCode)
	}

	if err := s.UpdatePolicy(nil); err == nil {
		t.Error("UpdatePolicy(nil) must be rejected")
	}
}

// A decider replaces the local policy for verdicts: the answer comes from the
// callback, and a callback that fails is a denial, not an allow.
func TestDecider_AnswersReplaceThePolicy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, upPort, _ := net.SplitHostPort(upURL.Host)

	var calls atomic.Int64
	// The compiled policy is not the verdict source here — it only feeds the
	// address check, which still refuses loopback for a name and honours an
	// explicit literal. The test server is a literal, so it must be listed.
	compiled, err := Parse(Policy{AllowedDomains: []string{"127.0.0.1:" + upPort}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Options{
		Policy: compiled,
		Decide: func(ctx context.Context, host string, port int) (Verdict, error) {
			calls.Add(1)
			if host == "127.0.0.1" && port == atoi(upPort) {
				return Verdict{Allow: true, Code: CodeAllowed, Reason: "decider said yes"}, nil
			}
			return Verdict{Code: CodeNotAllowed, Reason: "decider said no"}, nil
		},
		DecideCacheTTL: 0,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	client := proxyClient(t, srv)
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("allowed request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decider-allowed request: status=%d", resp.StatusCode)
	}

	resp, err = client.Get("http://example.org/")
	if err != nil {
		t.Fatalf("denied request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("decider-denied request: status=%d, want 403", resp.StatusCode)
	}
	if calls.Load() < 2 {
		t.Errorf("the decider was called %d times, expected one per connection", calls.Load())
	}
}

// No answer is no permission.
func TestDecider_ErrorIsADenial(t *testing.T) {
	compiled, _ := Parse(Policy{})
	srv, err := Start(Options{
		Policy: compiled,
		Decide: func(ctx context.Context, host string, port int) (Verdict, error) {
			return Verdict{}, errors.New("control plane is down")
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	client := proxyClient(t, srv)
	resp, err := client.Get("http://example.org/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 when the decider fails", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Deny-Reason"); got != CodeDeciderUnavailable {
		t.Errorf("X-Deny-Reason = %q, want %q", got, CodeDeciderUnavailable)
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
