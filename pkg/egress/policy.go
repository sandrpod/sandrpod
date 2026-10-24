// Copyright 2026 SandrPod
// Egress policy: the allow-list that decides whether a sandboxed process may
// open a connection, and the reason it is told when it may not.
//
// This is the policy half only — matching names, ports and CIDRs. Enforcement
// lives in proxy.go (HTTP) and socks.go (SOCKS5); the address check that stops
// an allow-listed name from resolving into loopback or a cloud metadata
// endpoint lives in guard.go.
//
// The model is deliberately the same shape as Anthropic's sandbox-runtime
// (network.allowedDomains / deniedDomains / deniedDomainReasons), because the
// semantics there have been thought through at length and a second, subtly
// different dialect would only cost us operator confusion:
//
//   - allow-only: an empty allowed_domains means no egress at all;
//   - deny is evaluated first and is unconditional — a denial can never be
//     overridden by a broader allow;
//   - an entry is a DNS name, a `*.` wildcard, or an IP literal, each with an
//     optional `:port` suffix that pins it to that destination port;
//   - a denial carries a reason written for the model, not for a log file.
//
// Vocabulary note: "domain" here means "destination as written", which may be
// an IP literal. Guarding against what a name *resolves to* is a separate
// decision, made at dial time.
package egress

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Policy is the allow-list applied to every connection a sandboxed process
// makes through the proxy.
//
// Field names are the on-disk JSON contract (snake_case, same as
// permissions.json). A policy file that fails to parse or validate is a
// startup error, never a silent fallback to "allow everything": a typo in a
// deny entry must not widen the sandbox.
type Policy struct {
	// AllowedDomains are the destinations that may be reached. Empty means
	// no egress at all. Examples: "api.github.com", "*.npmjs.org",
	// "registry.npmjs.org:443", "127.0.0.1:8080", "[::1]:8080".
	AllowedDomains []string `json:"allowed_domains"`

	// DeniedDomains are checked before AllowedDomains and deny
	// unconditionally. "*" denies everything (the default posture).
	DeniedDomains []string `json:"denied_domains,omitempty"`

	// DeniedDomainReasons maps a DeniedDomains entry (matched by exact
	// string) to the reason reported back in place of the generic one. This
	// is the field that turns a block into something a model can act on:
	// "ssh to github is blocked; use an https remote".
	DeniedDomainReasons map[string]string `json:"denied_domain_reasons,omitempty"`

	// DeniedResolvedAddresses are extra CIDRs or IPs that an allowed
	// hostname must not resolve to: 10.0.0.0/8, 192.168.0.0/16, fd00::/7.
	// Private space is not denied by default — an intranet hostname is a
	// legitimate thing to allow — so an operator who wants it lists it.
	DeniedResolvedAddresses []string `json:"denied_resolved_addresses,omitempty"`

	// StrictAllowlist denies anything that no allow entry covers. It exists
	// for symmetry with sandbox-runtime, where a host matching neither list
	// can otherwise fall through to an interactive "ask". SandrPod has no
	// per-connection ask yet, so the default is already to deny; setting
	// this makes that explicit and survives a future ask callback.
	StrictAllowlist bool `json:"strict_allowlist,omitempty"`
}

// Rule is one parsed allow/deny entry.
//
// Exported so a test — or a future `sandrpod-tray policy` subcommand — can
// print exactly what the proxy will enforce, rather than re-parsing the JSON.
type Rule struct {
	// Raw is the entry as written, which is what a denial reports back.
	Raw string
	// Host is the lower-case name or IP literal, without brackets or port.
	Host string
	// Port is the pinned port, or 0 when the entry matches any port.
	Port int
	// Wildcard is true for a `*.example.com` entry.
	Wildcard bool
	// IsIP is true when Host is an IP literal (v4 or v6).
	IsIP bool
}

// Verdict is the outcome of a policy decision.
type Verdict struct {
	// Allow is the only thing the caller must honour. Every error path in
	// this package returns Allow=false.
	Allow bool
	// Code is a short machine-readable label: "allowed", "denied_domain",
	// "not_allowed", "denied_address". It goes on the wire as
	// X-Deny-Reason for HTTP and as the SOCKS5 reply code's companion in the
	// audit log.
	Code string
	// Rule is the entry that decided, as written in the policy.
	Rule string
	// Reason is the human/model-facing explanation. Written as one line of
	// plain English: a model reading it should learn what was blocked and
	// what to do instead.
	Reason string
	// Host and Port are the destination the verdict is about.
	Host string
	Port int
	// Address is the resolved address, set only by the address check. The
	// reason line names the class (loopback, metadata, this host's own) and
	// leaves the address to the audit log.
	Address string
}

// Codes reported in Verdict.Code and in audit events. Stable on the wire.
const (
	CodeAllowed       = "allowed"
	CodeDeniedDomain  = "denied_domain"
	CodeNotAllowed    = "not_allowed"
	CodeDeniedAddress = "denied_address"
	// CodeDeciderUnavailable is reported when a remote decider could not answer
	// in time. It is a denial, and it says so: a client must be able to tell
	// "policy said no" from "policy did not answer" when it reads the log.
	CodeDeciderUnavailable = "decider_unavailable"
)

// Parse validates and compiles a policy. It returns an error rather than a
// partial policy: a malformed entry means the operator's intent is unknown,
// and guessing which half of a half-broken policy to apply is how allow-lists
// become allow-everything.
func Parse(p Policy) (*Compiled, error) {
	c := &Compiled{policy: p}
	var err error

	if c.allowed, err = compileRules(p.AllowedDomains, "allowed_domains"); err != nil {
		return nil, err
	}
	if c.denied, err = compileRules(p.DeniedDomains, "denied_domains"); err != nil {
		return nil, err
	}
	for _, entry := range p.DeniedResolvedAddresses {
		cidr, cerr := parseCIDR(entry)
		if cerr != nil {
			return nil, fmt.Errorf("egress policy: denied_resolved_addresses: %w", cerr)
		}
		c.deniedAddrs = append(c.deniedAddrs, cidr)
	}
	return c, nil
}

// LoadPolicy reads and compiles a policy file. Unknown fields are an error:
// a misspelled key ("allowed_domain") would otherwise silently produce an
// empty allow-list, which reads at runtime exactly like a working policy that
// denies everything.
func LoadPolicy(path string) (*Compiled, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("egress policy: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("egress policy %s: %w", path, err)
	}
	// A second JSON value means two policies in one file, which is always a
	// mistake (an editor appended instead of replaced).
	if err := ensureEOF(dec); err != nil {
		return nil, fmt.Errorf("egress policy %s: %w", path, err)
	}
	return Parse(p)
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing data after the policy object")
		}
		return err
	}
	return nil
}

// Compiled is a Policy with its entries parsed. Immutable and safe for
// concurrent use; the proxy consults it per connection.
type Compiled struct {
	policy      Policy
	allowed     []Rule
	denied      []Rule
	deniedAddrs []cidrRule
}

// Policy returns the policy as it was written, for reporting and for a
// future hot-reload diff.
func (c *Compiled) Policy() Policy { return c.policy }

// AllowedRules exposes the compiled allow entries, for diagnostics.
func (c *Compiled) AllowedRules() []Rule { return append([]Rule(nil), c.allowed...) }

// Decide applies the policy to a destination that has NOT been resolved yet.
// The address check happens separately (guard.go), because only it knows what
// the name resolves to.
func (c *Compiled) Decide(host string, port int) Verdict {
	h := canonicalHost(host)

	// 1. Explicit denial wins, always, before any allow is considered.
	for _, r := range c.denied {
		if r.matches(h, port) {
			reason := c.policy.DeniedDomainReasons[r.Raw]
			if reason == "" {
				reason = fmt.Sprintf("%s:%d is on the egress deny list", h, port)
			}
			return Verdict{Code: CodeDeniedDomain, Rule: r.Raw, Reason: reason, Host: h, Port: port}
		}
	}

	// 2. An allow entry lets it through to the address check.
	if r, ok := c.matchAllow(h, port); ok {
		return Verdict{Allow: true, Code: CodeAllowed, Rule: r.Raw, Host: h, Port: port}
	}

	// 3. Nothing decided it: deny. SandrPod has no per-connection prompt
	//    yet, so "undecided" is indistinguishable from "not allowed", and the
	//    reason says what to add to the policy.
	reason := fmt.Sprintf("%s:%d is not on the egress allow list", h, port)
	if c.policy.StrictAllowlist {
		reason = fmt.Sprintf("%s:%d is not allowed (strict allowlist)", h, port)
	}
	return Verdict{Code: CodeNotAllowed, Reason: reason, Host: h, Port: port}
}

// AllowsLiteral reports whether the policy allow-lists this exact IP literal
// and port. The guard uses it to honour an explicit operator choice: naming
// 127.0.0.1:3000 in the allow list is a decision, not an accident, so the
// loopback check does not second-guess it.
func (c *Compiled) AllowsLiteral(host string, port int) bool {
	h := canonicalHost(host)
	for _, r := range c.allowed {
		if r.IsIP && r.matches(h, port) {
			return true
		}
	}
	return false
}

// DeniedAddressReason reports whether an IP is in denied_resolved_addresses.
func (c *Compiled) DeniedAddressReason(ip netip.Addr) (string, bool) {
	for _, r := range c.deniedAddrs {
		if r.contains(ip) {
			return r.raw, true
		}
	}
	return "", false
}

func (c *Compiled) matchAllow(host string, port int) (Rule, bool) {
	// Most specific first would be nice, but a plain list is what
	// sandbox-runtime does and what operators expect; the first match wins
	// and entries are few.
	for _, r := range c.allowed {
		if r.matches(host, port) {
			return r, true
		}
	}
	return Rule{}, false
}

// matches reports whether the rule covers host:port.
func (r Rule) matches(host string, port int) bool {
	if r.Port != 0 && r.Port != port {
		return false
	}
	if r.Host == "*" {
		return true
	}
	if r.IsIP {
		return r.Host == host
	}
	if r.Wildcard {
		suffix := r.Host[1:] // ".example.com"
		// `*.example.com` covers a.example.com and a.b.example.com but not
		// example.com itself — matching sandbox-runtime, and matching what
		// an operator writing "*.example.com" means.
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return r.Host == host
}

func compileRules(entries []string, field string) ([]Rule, error) {
	out := make([]Rule, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		r, err := compileRule(raw)
		if err != nil {
			return nil, fmt.Errorf("egress policy: %s: %w", field, err)
		}
		if _, dup := seen[r.Raw]; dup {
			continue
		}
		seen[r.Raw] = struct{}{}
		out = append(out, r)
	}
	return out, nil
}

// compileRule parses one entry: [scheme-free] host[:port], with `*.` wildcards
// and bracketed IPv6 literals.
func compileRule(raw string) (Rule, error) {
	entry := strings.TrimSpace(raw)
	if entry == "" {
		return Rule{}, fmt.Errorf("empty entry")
	}
	if strings.ContainsAny(entry, "/ \t") {
		return Rule{}, fmt.Errorf("%q: entries are host[:port], not URLs or paths", raw)
	}
	r := Rule{Raw: entry}

	host, portStr, err := splitHostPort(entry)
	if err != nil {
		return Rule{}, fmt.Errorf("%q: %w", raw, err)
	}
	if portStr != "" {
		p, perr := strconv.Atoi(portStr)
		if perr != nil || p < 1 || p > 65535 {
			return Rule{}, fmt.Errorf("%q: bad port %q", raw, portStr)
		}
		r.Port = p
	}

	host = strings.ToLower(host)
	if host == "*" {
		r.Host = "*"
		r.Wildcard = true
		return r, nil
	}
	if strings.HasPrefix(host, "*.") {
		if len(host) <= 3 {
			return Rule{}, fmt.Errorf("%q: wildcard needs a domain after it", raw)
		}
		r.Host = host[1:]
		r.Wildcard = true
		return r, nil
	}
	if strings.Contains(host, "*") {
		return Rule{}, fmt.Errorf("%q: `*` is only supported as a leading `*.` label", raw)
	}
	if isIPLiteral(host) {
		r.Host = canonicalHost(host)
		r.IsIP = true
		return r, nil
	}
	if err := validateHostname(host); err != nil {
		return Rule{}, fmt.Errorf("%q: %w", raw, err)
	}
	r.Host = host
	return r, nil
}

// splitHostPort splits "host", "host:443", "[::1]", "[::1]:8080".
func splitHostPort(s string) (host, port string, err error) {
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", "", fmt.Errorf("unclosed `[` in IPv6 literal")
		}
		host = s[1:end]
		rest := s[end+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", fmt.Errorf("unexpected %q after `]`", rest)
		}
		return host, rest[1:], nil
	}
	if strings.Count(s, ":") > 1 {
		// An unbracketed multi-colon entry is an IPv6 address with an
		// ambiguous tail: "2001:db8::1:443" is itself a valid address.
		return "", "", fmt.Errorf("IPv6 literals with a port must be bracketed: [%s]", s)
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:], nil
	}
	return s, "", nil
}

// validateHostname accepts plain DNS names: dot-separated labels of letters,
// digits and inner hyphens. Anything else is a configuration mistake worth
// reporting at load time rather than at 3am.
func validateHostname(h string) error {
	if h == "" {
		return fmt.Errorf("empty host")
	}
	if len(h) > 253 {
		return fmt.Errorf("host is longer than 253 bytes")
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" {
			return fmt.Errorf("empty label")
		}
		if len(label) > 63 {
			return fmt.Errorf("label %q is longer than 63 bytes", label)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label %q starts or ends with `-`", label)
		}
		for _, c := range label {
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			default:
				return fmt.Errorf("label %q has an unsupported character %q", label, c)
			}
		}
	}
	return nil
}

// canonicalHost lower-cases and strips IPv6 brackets, so a policy entry and a
// client's request compare equal however each was spelled.
func canonicalHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return strings.TrimSuffix(h, ".")
}

// Strings returns the policy's allow and deny entries, sorted, for a startup
// log line an operator can eyeball without opening the file.
func (c *Compiled) Strings() (allow, deny []string) {
	for _, r := range c.allowed {
		allow = append(allow, r.Raw)
	}
	for _, r := range c.denied {
		deny = append(deny, r.Raw)
	}
	sort.Strings(allow)
	sort.Strings(deny)
	return allow, deny
}
