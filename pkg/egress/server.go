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
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
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
	// HTTPPort and SOCKSPort pin the listening ports (0 = ephemeral). A
	// container pod needs fixed ports because the sandbox's environment is
	// built before the proxy exists; an agent can leave them ephemeral and
	// read the port back from the server.
	HTTPPort  int
	SOCKSPort int
	// Decide, when set, replaces the local policy as the *verdict* source: the
	// proxy asks it per connection instead of consulting the compiled policy.
	// That is what a control-plane-decided sandbox is — the rules live at the
	// server, the agent holds none, and a tightened policy applies to the next
	// connection rather than the next poll.
	//
	// The compiled Policy still applies to the address check (which IP literals
	// are explicitly allowed, which CIDRs are refused), because that part is
	// about what a name may resolve to and needs no round trip.
	//
	// An error or a timeout is a denial. A decider that cannot answer must not
	// be a decider that allows.
	Decide func(ctx context.Context, host string, port int) (Verdict, error)
	// DecideTimeout bounds one decider call. Default 5s.
	DecideTimeout time.Duration
	// DecideCacheTTL caches verdicts per destination. Zero disables caching.
	// A small TTL keeps a loop of requests from becoming a loop of round
	// trips, at the cost of a policy change landing up to that much later.
	DecideCacheTTL time.Duration
	// UnixSocketPath makes the HTTP proxy listen on a unix socket instead of
	// TCP. That is what a sandbox with its own network namespace needs: it has
	// loopback and nothing else, so a TCP listener on the host is unreachable,
	// while a socket bind-mounted into the namespace is a file the sandbox can
	// open. SOCKS stays off in this mode (SOCKS clients dial TCP).
	UnixSocketPath string
	// ListenHost is the interface both proxies bind to. Empty means 127.0.0.1,
	// which is what an agent wants. A container pod binds the gateway address
	// of its per-pod internal network instead, because that is the only
	// address inside the sandbox that leads anywhere.
	//
	// A non-loopback bind is only allowed together with a token: the proxy has
	// no other authentication, and a token-less listener on a reachable address
	// is an open relay. Token is generated when empty, so this only rejects the
	// explicit Token == "" + off-loopback combination the caller has to ask for.
	ListenHost string
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
	// unixSocket is set when the HTTP proxy listens on a unix socket instead of
	// TCP (a sandbox with its own network namespace).
	unixSocket string
	// verdicts caches decider answers when Decide is set.
	verdictMu sync.Mutex
	verdicts  map[string]cachedVerdict

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

	if opts.Decide != nil {
		if opts.DecideTimeout <= 0 {
			opts.DecideTimeout = 5 * time.Second
		}
	}
	s := &Server{
		opts:     opts,
		token:    token,
		dialer:   newDialer(opts.Policy, opts.DialTimeout),
		verdicts: make(map[string]cachedVerdict),
	}
	s.policy.Store(opts.Policy)
	s.transport = s.newTransport()

	listenHost := opts.ListenHost
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}
	if opts.UnixSocketPath != "" {
		// Remove a stale socket left by a previous process; a live one will still
		// be in use, and net.Listen will report that below.
		if err := os.Remove(opts.UnixSocketPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("egress: clear %s: %w", opts.UnixSocketPath, err)
		}
		httpLn, err := net.Listen("unix", opts.UnixSocketPath)
		if err != nil {
			return nil, fmt.Errorf("egress: listen on %s: %w", opts.UnixSocketPath, err)
		}
		s.httpLn = httpLn
		s.unixSocket = opts.UnixSocketPath
	} else {
		httpLn, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(opts.HTTPPort)))
		if err != nil {
			return nil, fmt.Errorf("egress: listen http: %w", err)
		}
		s.httpLn = httpLn

		socksLn, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(opts.SOCKSPort)))
		if err != nil {
			httpLn.Close()
			return nil, fmt.Errorf("egress: listen socks: %w", err)
		}
		s.socksLn = socksLn
	}

	s.httpSrv = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: 15 * time.Second,
		// No read/write timeout on the whole connection: a CONNECT tunnel and
		// a long-poll response are both legitimately idle for a long time.
	}
	go func() { _ = s.httpSrv.Serve(s.httpLn) }()
	if s.socksLn != nil {
		go s.acceptSOCKS()
	}

	return s, nil
}

// cachedVerdict is one decider answer held for DecideCacheTTL.
type cachedVerdict struct {
	verdict Verdict
	until   time.Time
}

// decide returns the verdict for a destination: the decider's when one is
// installed, the compiled policy's otherwise. Every failure path is a denial
// with a reason, because "the decider timed out" must not read as "allowed".
func (s *Server) decide(ctx context.Context, host string, port int) Verdict {
	if s.opts.Decide == nil {
		return s.policy.Load().Decide(host, port)
	}
	key := host + ":" + itoa(port)
	if ttl := s.opts.DecideCacheTTL; ttl > 0 {
		s.verdictMu.Lock()
		if c, ok := s.verdicts[key]; ok && time.Now().Before(c.until) {
			s.verdictMu.Unlock()
			return c.verdict
		}
		s.verdictMu.Unlock()
	}

	dctx, cancel := context.WithTimeout(ctx, s.opts.DecideTimeout)
	defer cancel()
	v, err := s.opts.Decide(dctx, host, port)
	if err != nil {
		return Verdict{Code: CodeDeciderUnavailable, Host: host, Port: port,
			Reason: "the policy decider did not answer; the connection is refused rather than allowed"}
	}
	if v.Host == "" {
		v.Host = host
	}
	if v.Port == 0 {
		v.Port = port
	}
	if s.opts.DecideCacheTTL > 0 {
		s.verdictMu.Lock()
		s.verdicts[key] = cachedVerdict{verdict: v, until: time.Now().Add(s.opts.DecideCacheTTL)}
		s.verdictMu.Unlock()
	}
	return v
}

func itoa(n int) string { return strconv.Itoa(n) }

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
	// The socket file is not removed by closing the listener.
	if s.unixSocket != "" {
		_ = os.Remove(s.unixSocket)
	}
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	return first
}

// HTTPAddr is the host:port of the HTTP proxy.
func (s *Server) HTTPAddr() string { return s.httpLn.Addr().String() }

// SOCKSAddr is the host:port of the SOCKS5 proxy, or "" when the proxy listens
// on a unix socket (SOCKS v1 has no unix-socket form).
func (s *Server) SOCKSAddr() string {
	if s.socksLn == nil {
		return ""
	}
	return s.socksLn.Addr().String()
}

// UnixSocket is the HTTP proxy's socket path, when it listens on one.
func (s *Server) UnixSocket() string { return s.unixSocket }

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
