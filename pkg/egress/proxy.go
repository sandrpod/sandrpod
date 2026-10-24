// Copyright 2026 SandrPod
// The HTTP half of the egress proxy.
//
// Two request shapes arrive here, and they are handled differently on purpose:
//
//   - absolute-form ("GET http://host/path HTTP/1.1"), which is plain HTTP.
//     The request is re-issued through a Transport whose dial is the guarded
//     one, so the policy and the address check both see the real destination.
//     Response bytes are streamed, not buffered, so SSE and long polls behave
//     as they do without a proxy.
//
//   - CONNECT ("CONNECT host:443"), which is anything TLS. The tunnel is
//     opaque: SandrPod filters the destination, not the bytes. TLS
//     interception with a per-sandbox CA is a separate feature (the credential
//     masking it enables is on the roadmap, not in this build), and pretending
//     otherwise would be worse than saying so.
//
// A refusal is answered by the proxy itself, with:
//
//	403, X-Deny-Reason: <code>, body = <reason>
//
// The body is written for the model: "example.org:443 is not on the egress
// allow list — add it to the sandbox's egress policy if this is intended".
// A tool that surfaces the body (curl does) therefore teaches the agent what
// to do instead of leaving it to retry.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// hopByHop are never forwarded in either direction (RFC 7230 §6.1), plus the
// proxy credentials themselves.
var hopByHop = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r.Header.Get("Proxy-Authorization")) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="sandrpod-egress"`)
		s.writeDeny(w, http.StatusProxyAuthRequired, "proxy_auth_required",
			"this proxy needs its sandbox token: use the HTTP_PROXY/HTTPS_PROXY values the sandbox was started with")
		return
	}

	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	s.handleForward(w, r)
}

// handleForward re-issues a plain-HTTP request to its absolute-form target.
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Host == "" {
		s.writeDeny(w, http.StatusBadRequest, "bad_request",
			"the proxy expects an absolute request URI (http://host/path)")
		return
	}
	host, port := hostPort(r.URL.Host, 80)

	verdict := s.opts.Policy.Decide(host, port)
	if !verdict.Allow {
		s.deny(w, CodeHTTP, r, verdict, "")
		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = "" // client-side requests carry none
	out.URL.Scheme = "http"
	out.URL.Host = r.URL.Host
	stripHopByHop(out.Header)

	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		s.fail(w, r, host, port, err)
		return
	}
	defer resp.Body.Close()
	s.record(Event{Kind: CodeHTTP, Host: host, Port: port, Code: CodeAllowed,
		Rule: verdict.Rule, Method: r.Method})

	for k, vv := range resp.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
}

// handleConnect tunnels a TCP connection to the CONNECT target.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port := hostPort(r.Host, 443)

	verdict := s.opts.Policy.Decide(host, port)
	if !verdict.Allow {
		s.deny(w, CodeConnect, r, verdict, "")
		return
	}

	conn, err := s.dialer.dialAllowingVetted(r.Context(), host, port)
	if err != nil {
		s.fail(w, r, host, port, err)
		return
	}
	s.record(Event{Kind: CodeConnect, Host: host, Port: port, Code: CodeAllowed,
		Rule: verdict.Rule, Method: r.Method})

	hj, ok := w.(http.Hijacker)
	if !ok {
		conn.Close()
		s.writeDeny(w, http.StatusInternalServerError, "no_hijack",
			"the proxy cannot take over this connection")
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		conn.Close()
		return
	}

	// Anything the client already buffered past the request line belongs to
	// the tunnelled stream; brw.Reader still holds it.
	if n := brw.Reader.Buffered(); n > 0 {
		if _, err := io.CopyN(conn, brw, int64(n)); err != nil {
			client.Close()
			conn.Close()
			return
		}
	}

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		conn.Close()
		return
	}

	s.splice(client, conn)
}

// transport dials through the guard, and never through another proxy: a
// Transport with Proxy=nil does not read HTTP_PROXY from the environment, so
// the chain cannot loop back into itself.
func (s *Server) newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port := hostPort(addr, 80)
			// The request path already decided this; deciding again here is
			// what makes a pooled connection impossible to establish without
			// a verdict, whatever a future caller does.
			if v := s.opts.Policy.Decide(host, port); !v.Allow {
				return nil, &DenyError{Verdict: v}
			}
			return s.dialer.dialAllowingVetted(ctx, host, port)
		},
	}
}

// fail turns a dial error into a response: a policy refusal becomes 403 with
// the reason, anything else becomes a 502 that says the failure was the
// network's, not the policy's. That distinction is the difference between a
// model retrying forever and a model changing approach.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	if deny, ok := asDenyError(err); ok {
		s.record(Event{
			Kind: kindOf(r), Host: host, Port: port, Code: deny.Verdict.Code,
			Reason: deny.Verdict.Reason, Address: deny.Verdict.Address, Method: r.Method,
		})
		s.writeDeny(w, http.StatusForbidden, deny.Verdict.Code, deny.Verdict.Reason)
		return
	}
	s.writeDeny(w, http.StatusBadGateway, "upstream_unreachable",
		fmt.Sprintf("could not connect to %s:%d (%v)", host, port, err))
}

// deny answers a policy refusal: 403, the code in a header tools can read,
// and the reason in the body.
func (s *Server) deny(w http.ResponseWriter, kind string, r *http.Request, v Verdict, addr string) {
	s.record(Event{
		Kind: kind, Host: v.Host, Port: v.Port, Code: v.Code, Rule: v.Rule,
		Reason: v.Reason, Address: addr, Method: r.Method,
	})
	status := http.StatusForbidden
	if kind == CodeConnect {
		// A CONNECT never gets a body read by anything useful; the reason
		// still goes in the header, and curl prints it.
		status = http.StatusForbidden
	}
	s.writeDeny(w, status, v.Code, v.Reason)
}

func (s *Server) writeDeny(w http.ResponseWriter, status int, code, reason string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Deny-Reason", code)
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reason+"\n")
}

// splice copies bytes both ways until both directions are done, then closes
// both ends. Half-close is preserved so a client that finishes its request
// still reads the response.
func (s *Server) splice(client, upstream net.Conn) {
	defer client.Close()
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); closeWrite(upstream); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); closeWrite(client); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// streamCopy copies a response body to the client, flushing as it goes so
// chunked/SSE responses are not held until the end.
func streamCopy(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func stripHopByHop(h http.Header) {
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}
	// Connection may also name additional hop-by-hop headers.
	for _, v := range h.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if name := strings.TrimSpace(token); name != "" {
				h.Del(name)
			}
		}
	}
}

// hostPort splits "host:port", falling back to defPort.
func hostPort(addr string, defPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.Trim(addr, "[]"), defPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		port = defPort
	}
	return host, port
}

// kindOf labels an event by the request shape that produced it.
func kindOf(r *http.Request) string {
	if r.Method == http.MethodConnect {
		return CodeConnect
	}
	return CodeHTTP
}

var errClosed = errors.New("egress proxy closed")
