// Copyright 2026 SandrPod Contributors
// The terminating path: a CONNECT the policy allowed, terminated by us, with
// credentials substituted on the way out.
//
// Flow for one terminated tunnel:
//
//	client --TLS(leaf for host, signed by our CA)--> proxy --TLS(verified)--> upstream
//	                       ^                                  ^
//	              sentinel in the request            real value substituted here
//
// The client's request is parsed by an http.Server running on the TLS
// connection, rewritten (headers, body, SigV4 signature), and sent upstream
// through a Transport whose dialer is the guarded one — so the address check
// still applies, and a policy change still applies.
package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxBodyForMasking bounds how much of a request body is read to look for
// sentinels. Beyond it the request is refused when masking is on: forwarding a
// body unexamined would mean a credential we were asked to mask goes out in the
// clear of our substitution.
const maxBodyForMasking = 16 << 20

// terminate handshakes TLS with the client and serves its requests through the
// masking handler.
func (s *Server) terminate(w http.ResponseWriter, r *http.Request, host string, port int) {
	ca := s.opts.CA
	if ca == nil {
		s.writeDeny(w, http.StatusInternalServerError, "no_ca",
			"the proxy is configured to terminate TLS but has no certificate authority")
		return
	}
	cert, err := ca.leafFor(host, s.opts.Terminate.LeafTTL)
	if err != nil {
		s.writeDeny(w, http.StatusBadGateway, "leaf_failed",
			"the proxy could not mint a certificate for this host")
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		s.writeDeny(w, http.StatusInternalServerError, "no_hijack",
			"the proxy cannot take over this connection")
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		return
	}

	tlsConn := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		// HTTP/1.1 only: the masking handler speaks HTTP/1.1, and negotiating
		// h2 would silently skip substitution for every request that used it.
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})

	addr := net.JoinHostPort(host, itoa(port))
	ln := &singleConnListener{conn: withBuffered(tlsConn, brw)}
	srv := &http.Server{
		Handler:           s.terminatedHandler(host, port),
		ReadHeaderTimeout: 30 * time.Second,
		// No global timeouts: a long download or an SSE stream is legitimate.
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return context.WithValue(ctx, terminatedHostKey{}, addr)
		},
	}
	_ = srv.Serve(ln)
}

// terminatedHostKey carries the CONNECT target into the handler.
type terminatedHostKey struct{}

// terminatedHandler builds the per-tunnel handler.
func (s *Server) terminatedHandler(host string, port int) http.Handler {
	tr := s.newTransport()
	roots := s.opts.Terminate.UpstreamRoots
	tr.TLSClientConfig = &tls.Config{
		ServerName: host,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serveTerminated(w, r, tr, host, port)
	})
}

// serveTerminated forwards one request upstream, substituting credentials.
func (s *Server) serveTerminated(w http.ResponseWriter, r *http.Request, tr *http.Transport, host string, port int) {
	mask := s.masker()

	// A request whose body cannot be examined is refused rather than forwarded
	// unexamined: the whole point is that the sentinel never leaves.
	var body []byte
	if r.Body != nil && r.ContentLength != 0 {
		limited := io.LimitReader(r.Body, maxBodyForMasking+1)
		b, err := io.ReadAll(limited)
		if err != nil {
			s.writeDeny(w, http.StatusBadRequest, "body_read",
				"the proxy could not read the request body")
			return
		}
		if len(b) > maxBodyForMasking && mask != nil {
			s.writeDeny(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("the request body exceeds the %d byte limit for credential masking; "+
					"split the upload or send it without a masked credential", maxBodyForMasking))
			return
		}
		if len(b) <= maxBodyForMasking {
			body = b
			r.Body = io.NopCloser(strings.NewReader(string(b)))
		} else {
			// Unmasked and oversized: stream it through unchanged.
			r.Body = io.NopCloser(io.MultiReader(strings.NewReader(string(b)), r.Body))
		}
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = "https"
	out.URL.Host = net.JoinHostPort(host, itoa(port))
	if port == 443 {
		out.URL.Host = host
	}
	// Keep the port the client used when it was not 443: an upstream that
	// builds absolute links from Host needs it, and dropping it would make a
	// terminated tunnel behave differently from an opaque one.
	if port == 443 {
		out.Host = host
	} else {
		out.Host = net.JoinHostPort(host, itoa(port))
	}
	out.Header = r.Header.Clone()

	// This is a re-originated request, not a relayed one: the proxy's own
	// headers (Proxy-Authorization carries the sandbox token) and the client's
	// hop-by-hop set must not travel upstream. Without this the upstream sees
	// the token, and `Connection: keep-alive` on a fresh connection is a
	// protocol lie.
	stripHopByHop(out.Header)

	// Refuse a cleartext hop: this path is only used when we terminated, so a
	// sentinel here means the client sent it over TLS and we substitute. The
	// plain-HTTP path has its own refusal (see serveHTTP).
	usedHeaders := mask.substituteHeaders(out.Header)
	usedBody := []string(nil)
	bodyChanged := false
	if len(body) > 0 {
		nb, changed, names := mask.Substitute(body)
		if changed {
			bodyChanged = true
			body = nb
			usedBody = names
			out.Body = io.NopCloser(strings.NewReader(string(body)))
			out.ContentLength = int64(len(body))
			out.Header.Set("Content-Length", itoa(len(body)))
		}
	}

	used := append(usedHeaders, usedBody...)
	if len(used) > 0 || s.masker() != nil {
		// A SigV4 signature over the request must be recomputed with the real
		// secret, which is the only credential the proxy cannot simply swap in
		// place: the signature covers the whole request.
		if bodyChanged {
			// Re-signing would need a signature over the rewritten body, and
			// the client signed the sentinel body. Fail closed instead of
			// sending a request the upstream will reject as a mismatch.
			if hasSigV4(out) {
				s.writeDeny(w, http.StatusConflict, "sigv4_body_rewritten",
					"the request carries a SigV4 signature and a body that changed under credential "+
						"masking; send the credential in a header instead of the body")
				return
			}
		} else if resigned, err := mask.ResignAWS(out); err != nil {
			s.writeDeny(w, http.StatusBadGateway, "sigv4_resign",
				"the proxy could not re-sign the request with the real credential: "+err.Error())
			return
		} else if resigned {
			used = append(used, "AWS SigV4")
		}
	}

	if len(used) > 0 {
		// Recorded before the round trip: the audit question is "did we put a
		// credential on a request", and the answer does not depend on whether
		// the upstream then answered.
		s.record(Event{Kind: CodeConnect, Host: host, Port: port, Code: CodeAllowed,
			Rule: "", Method: r.Method, Terminated: true, Credentials: used})
	}

	// The upstream must be dialled through the guard: an allowed name that
	// resolves to loopback or metadata is refused here exactly as it is on the
	// opaque path.
	resp, err := tr.RoundTrip(out)
	if err != nil {
		if deny, ok := asDenyError(err); ok {
			s.writeDeny(w, http.StatusForbidden, deny.Verdict.Code, deny.Verdict.Reason)
			s.record(Event{Kind: CodeConnect, Host: host, Port: port,
				Code: deny.Verdict.Code, Rule: deny.Verdict.Rule, Method: r.Method})
			return
		}
		s.writeDeny(w, http.StatusBadGateway, "upstream_failed",
			"the proxy reached "+host+" but the request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

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

	s.record(Event{Kind: CodeConnect, Host: host, Port: port, Code: CodeAllowed,
		Rule: "", Method: r.Method, Terminated: true})
}

func hasSigV4(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), awsSigV4Prefix+" ")
}

// singleConnListener hands one connection to http.Server and then ends. It is
// how a hijacked CONNECT becomes an http.Server without opening a port.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	served := false
	l.once.Do(func() { c = l.conn; served = true })
	if served {
		return c, nil
	}
	return nil, io.EOF
}

func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// withBuffered re-attaches bytes the client sent before the hijack: they are
// the beginning of the TLS ClientHello.
func withBuffered(c net.Conn, brw *bufio.ReadWriter) net.Conn {
	if brw == nil || brw.Reader.Buffered() == 0 {
		return c
	}
	return &bufferedConn{Conn: c, r: brw.Reader}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

// newBufferedConn re-attaches bytes already read from c that belong to the byte
// stream after the protocol handshake. Without it a pipelining client loses its
// first payload bytes: the handshake parser buffers more than the handshake.
func newBufferedConn(c net.Conn, r *bufio.Reader) net.Conn {
	if r == nil || r.Buffered() == 0 {
		return c
	}
	return &bufferedConn{Conn: c, r: r}
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

var errNoCA = errors.New("egress: TLS termination requires a CA")

// carriesSentinel reports whether a cleartext request carries any masked
// credential: in a header, or in a body small enough to have been read.
func carriesSentinel(r *http.Request) bool {
	for k, vals := range r.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			if HasSentinel([]byte(v)) {
				return true
			}
		}
	}
	// A body is only inspectable when the caller kept it; the plain-HTTP path
	// does not buffer, so a sentinel in a body arrives as an unconverted
	// credential and the upstream rejects it. Refusing on headers is what makes
	// the failure immediate and legible.
	return false
}
