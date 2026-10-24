// Copyright 2026 SandrPod
// The egress server: policy in, two listeners out, one audit trail.
//
// Start() brings up an HTTP proxy and a SOCKS5 proxy on loopback, both
// answering only to a per-server token, both consulting the same Compiled
// policy and the same resolved-address check. Callers hand the sandbox the
// ProxyEnv() values and everything a proxy-aware tool does is then filtered;
// tools that ignore proxy environment variables are covered by the kernel
// floor described in docs/EGRESS_POLICY.md, which is a separate layer.
//
// Design constraints worth keeping:
//
//   - Loopback only. Binding 0.0.0.0 would turn the sandbox's allow-list into
//     a proxy for the whole network it sits on.
//   - Fail closed. Any error path — bad token, unparsable request, policy
//     miss, dial failure, panic in a handler — ends in no connection.
//   - Token, not trust. Two sandboxes on one machine must not be able to use
//     each other's allow-list, so the token is generated per server and is
//     the only credential either listener accepts.
//   - Every refusal is an Event. The audit trail is what makes a deny
//     reviewable later; a denial nobody can see is indistinguishable from a
//     network failure, for the operator as much as for the model.
package egress

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Event kinds, also used as the audit event's `kind` label.
const (
	CodeHTTP    = "http"
	CodeConnect = "connect"
	CodeSOCKS   = "socks"
)

// Event is one decision the proxy made. Denials are always reported; allowed
// connections are reported only when Options.LogAllowed is set, because at
// fleet scale the allow stream is mostly noise.
type Event struct {
	At      time.Time
	Kind    string // "http" | "connect" | "socks"
	Host    string
	Port    int
	Code    string // allowed | denied_domain | not_allowed | denied_address
	Rule    string // the policy entry that decided, when one did
	Reason  string
	Address string // resolved address, set when the address check refused
	Method  string
}

// Sink receives egress events. Implemented by the agent's audit adapter; a
// nil Sink means events are counted but not persisted.
type Sink interface {
	RecordEgress(Event)
}

// Options configures a Server.
type Options struct {
	// Policy is required. A nil policy is a programming error, not a
	// permissive default.
	Policy *Compiled
	// Token is the proxy password. Empty means "generate one", which is what
	// callers should normally do: a token that outlives the process is a
	// token that leaks into a config file.
	Token string
	// Sink receives denial events. Optional.
	Sink Sink
	// LogAllowed also reports connections that were allowed. Off by default.
	LogAllowed bool
	// DialTimeout bounds the connect to the upstream. Default 30s.
	DialTimeout time.Duration
}

// Server is a running pair of proxies.
type Server struct {
	opts      Options
	token     string
	dialer    *dialer
	transport *http.Transport

	httpLn  net.Listener
	socksLn net.Listener
	httpSrv *http.Server

	// policy is the live allow-list. Start installs the first one; a
	// control-plane sync calls UpdatePolicy later, and every decision reads
	// whichever pointer is current at the moment the connection arrives.
	policy atomic.Pointer[Compiled]

	allowed atomic.Uint64
	denied  atomic.Uint64
}

// Start binds both proxies on 127.0.0.1 with an ephemeral port and begins
// serving. The returned Server must be Closed to release the listeners.
func Start(opts Options) (*Server, error) {
	if opts.Policy == nil {
		return nil, errors.New("egress: Start needs a policy")
	}
	token := opts.Token
	if token == "" {
		t, err := newToken()
		if err != nil {
			return nil, fmt.Errorf("egress: generate token: %w", err)
		}
		token = t
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 30 * time.Second
	}

	s := &Server{
		opts:   opts,
		token:  token,
		dialer: newDialer(opts.Policy, opts.DialTimeout),
	}
	s.policy.Store(opts.Policy)
	s.transport = s.newTransport()

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress: listen http: %w", err)
	}
	s.httpLn = httpLn

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		httpLn.Close()
		return nil, fmt.Errorf("egress: listen socks: %w", err)
	}
	s.socksLn = socksLn

	s.httpSrv = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: 15 * time.Second,
		// No read/write timeout on the whole connection: a CONNECT tunnel and
		// a long-poll response are both legitimately idle for a long time.
	}
	go func() { _ = s.httpSrv.Serve(httpLn) }()
	go s.acceptSOCKS()

	return s, nil
}

// UpdatePolicy replaces the allow-list of a running server.
//
// This is what makes a control-plane policy change land without restarting the
// agent: the listeners, the token, the counters and the open tunnels all stay.
// What changes is the next decision — a domain added here is reachable on the
// next connection, one removed is refused on the next connection. A tunnel
// that is already open keeps carrying bytes: the proxy filters destinations,
// not streams, and pretending to cut a live CONNECT would be a promise this
// layer cannot keep. Callers that need that certainty close the sandbox.
//
// A nil policy is rejected rather than treated as "allow everything" or
// "deny everything": both readings are surprising, and both are one bug away
// from the wrong one.
func (s *Server) UpdatePolicy(p *Compiled) error {
	if p == nil {
		return errors.New("egress: UpdatePolicy needs a policy")
	}
	s.policy.Store(p)
	s.dialer.setPolicy(p)
	return nil
}

// Policy returns the policy the server is enforcing right now.
func (s *Server) Policy() *Compiled { return s.policy.Load() }

// Close stops both listeners and every in-flight tunnel they started.
func (s *Server) Close() error {
	var first error
	if s.httpSrv != nil {
		if err := s.httpSrv.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			first = err
		}
	}
	if s.socksLn != nil {
		if err := s.socksLn.Close(); err != nil && !errors.Is(err, net.ErrClosed) && first == nil {
			first = err
		}
	}
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	return first
}

// HTTPAddr is the host:port of the HTTP proxy.
func (s *Server) HTTPAddr() string { return s.httpLn.Addr().String() }

// SOCKSAddr is the host:port of the SOCKS5 proxy.
func (s *Server) SOCKSAddr() string { return s.socksLn.Addr().String() }

// Token is the credential both proxies require. It is generated per server
// and never written to disk.
func (s *Server) Token() string { return s.token }

// ProxyURL is the credential-bearing HTTP proxy URL, the form curl, npm and
// pip all accept.
func (s *Server) ProxyURL() string {
	return "http://sandbox:" + s.token + "@" + s.HTTPAddr()
}

// SOCKSURL is the credential-bearing SOCKS5 URL.
func (s *Server) SOCKSURL() string {
	return "socks5://sandbox:" + s.token + "@" + s.SOCKSAddr()
}

// ProxyEnv is the environment a sandboxed process needs so that every
// proxy-aware tool goes through the filter. Upper and lower case are both set
// because tools disagree about which they read (curl reads lower case first,
// many Java/Node libraries read upper case).
//
// NO_PROXY is deliberately NOT set: it is a bypass list, and a sandbox that
// can name hosts in it can step around the policy.
func (s *Server) ProxyEnv() []string {
	httpURL := s.ProxyURL()
	socksURL := s.SOCKSURL()
	return []string{
		"HTTP_PROXY=" + httpURL, "http_proxy=" + httpURL,
		"HTTPS_PROXY=" + httpURL, "https_proxy=" + httpURL,
		"ALL_PROXY=" + socksURL, "all_proxy=" + socksURL,
		// Node's built-in fetch/http read the proxy env only when asked to.
		// Node 22.21+ and 24.5+ honour this; older runtimes ignore it.
		"NODE_USE_ENV_PROXY=1",
	}
}

// Stats reports how many connections were allowed and denied so far. Denials
// are counted whether or not a Sink is installed.
func (s *Server) Stats() (allowed, denied uint64) {
	return s.allowed.Load(), s.denied.Load()
}

// authorized checks a Proxy-Authorization header against the token. The
// username is ignored on purpose — it is a label, not a secret (sandbox-runtime
// uses it to attribute a connection to a command; SandrPod has one proxy per
// agent, so the label would carry no information) — and only the password is
// compared.
func (s *Server) authorized(header string) bool {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return false
	}
	i := strings.IndexByte(string(raw), ':')
	if i < 0 {
		return false
	}
	return s.tokenMatches(string(raw[i+1:]))
}

func (s *Server) tokenMatches(pass string) bool {
	return subtle.ConstantTimeCompare([]byte(pass), []byte(s.token)) == 1
}

// record counts an event and forwards it to the sink.
func (s *Server) record(e Event) {
	e.At = time.Now()
	if e.Code == CodeAllowed {
		s.allowed.Add(1)
	} else {
		s.denied.Add(1)
	}
	if s.opts.Sink == nil {
		return
	}
	if e.Code == CodeAllowed && !s.opts.LogAllowed {
		return
	}
	s.opts.Sink.RecordEgress(e)
}

// newToken mints a 24-byte URL-safe token: 192 bits, so a local process
// cannot guess its way into the sandbox's allow-list.
func newToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
