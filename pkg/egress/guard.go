// Copyright 2026 SandrPod
// Egress address check: what an allow-listed name is actually allowed to
// resolve to.
//
// The allow list matches names. Whoever controls a permitted name's DNS — or
// any label under a permitted wildcard — controls what it resolves to, so
// "api.example.com is allowed" would otherwise also mean "whatever
// api.example.com points at today is allowed", including 127.0.0.1, the LAN
// and the cloud metadata service. The check here closes that: resolve once,
// drop every address in a denied class, dial one that survived, and never
// resolve again (a second lookup is a TOCTOU window and a rebinding vector).
//
// The denied classes are the ones that turn an allow-list into an SSRF
// primitive: loopback, unspecified, link-local, multicast, broadcast, the
// cloud metadata endpoints that live outside link-local, and every address
// currently assigned to one of this host's own interfaces (a service bound to
// 0.0.0.0 answers on the LAN address exactly as it does on loopback).
//
// Private-use space is NOT denied by default: an intranet hostname is a
// legitimate thing for an operator to allow. List it in
// denied_resolved_addresses when it is not.
package egress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// cidrRule is one parsed denied_resolved_addresses entry.
type cidrRule struct {
	raw    string
	prefix netip.Prefix
}

func (r cidrRule) contains(ip netip.Addr) bool {
	return r.prefix.Contains(ip.Unmap())
}

// parseCIDR accepts a bare IP or a CIDR, and normalises IPv4-mapped IPv6 into
// IPv4 so a rule written as 10.0.0.0/8 also covers ::ffff:10.0.0.1.
func parseCIDR(entry string) (cidrRule, error) {
	e := strings.TrimSpace(entry)
	if strings.Contains(e, "/") {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return cidrRule{}, fmt.Errorf("%q: %w", entry, err)
		}
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return cidrRule{}, fmt.Errorf("%q: an IPv4-mapped prefix must cover at least 96 bits", entry)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return cidrRule{raw: entry, prefix: p.Masked()}, nil
	}
	a, err := netip.ParseAddr(e)
	if err != nil {
		return cidrRule{}, fmt.Errorf("%q: %w", entry, err)
	}
	a = a.Unmap()
	return cidrRule{raw: entry, prefix: netip.PrefixFrom(a, a.BitLen())}, nil
}

// isIPLiteral reports whether a destination is an address rather than a name.
func isIPLiteral(host string) bool {
	_, err := netip.ParseAddr(canonicalHost(host))
	return err == nil
}

// metadataAddrs are the cloud instance-metadata / platform endpoints that do
// not sit in link-local, so the class check would miss them. Same set as
// sandbox-runtime's resolved-address guard.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("100.100.100.200"),    // Alibaba Cloud
	netip.MustParseAddr("168.63.129.16"),      // Azure wire server
	netip.MustParseAddr("192.0.0.192"),        // Oracle Cloud
	netip.MustParseAddr("169.254.169.254"),    // AWS / GCP / generic IMDS
	netip.MustParseAddr("fd00:ec2::254"),      // AWS IMDSv6
	netip.MustParseAddr("fd20:ce::254"),       // Azure IMDSv6
	netip.MustParseAddr("fd00:c1::a9fe:a9fe"), // Alibaba Cloud v6
	netip.MustParseAddr("fd00:42::42"),        // Tencent Cloud v6
}

// blockedClass names why an address is refused, for the reason line. The
// address itself is not reported: the model gets the class, the audit log
// keeps the address (see Event.Address).
func blockedClass(ip netip.Addr) string {
	switch {
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsUnspecified():
		return "the unspecified address"
	case ip.IsLinkLocalUnicast():
		return "a link-local address"
	case ip.IsMulticast():
		return "a multicast address"
	case ip.IsInterfaceLocalMulticast(), ip.IsLinkLocalMulticast():
		return "a multicast address"
	}
	for _, m := range metadataAddrs {
		if m == ip {
			return "a cloud metadata endpoint"
		}
	}
	return ""
}

// dialer resolves once, vets, and dials the vetted address.
type dialer struct {
	policy  *Compiled
	timeout time.Duration
	// lookup is injectable so the guard can be tested without a resolver.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)

	mu       sync.Mutex
	ownAddrs map[netip.Addr]struct{}
	ownAt    time.Time
}

// ownAddrCacheTTL bounds how long an interface list is trusted. Interfaces
// change (VPN up, Wi-Fi to Ethernet) and a stale set would either miss the
// host's new address or deny a dead one; 30s is cheap either way.
const ownAddrCacheTTL = 30 * time.Second

func newDialer(p *Compiled, timeout time.Duration) *dialer {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &dialer{
		policy:  p,
		timeout: timeout,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			return addrs, err
		},
	}
}

// dialAllowingVetted opens a TCP connection to host:port, refusing any
// destination whose resolved address falls in a denied class.
//
// The decision is made on the addresses this call resolved; the dial targets
// one of those exact addresses, so nothing between the check and the connect
// can change what gets reached.
func (d *dialer) dialAllowingVetted(ctx context.Context, host string, port int) (net.Conn, error) {
	ip, deny := d.vet(ctx, host, port)
	if deny != nil {
		return nil, deny
	}
	dctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	nd := net.Dialer{}
	conn, err := nd.DialContext(dctx, "tcp", net.JoinHostPort(ip.String(), fmt.Sprint(port)))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// vet returns the address that may be dialled, or a *DenyError.
func (d *dialer) vet(ctx context.Context, host string, port int) (netip.Addr, *DenyError) {
	// An IP literal in the allow list is an explicit operator decision:
	// "127.0.0.1:3000" in the policy means exactly that, so the class check
	// does not second-guess it. An IP literal that is merely *not* denied by
	// name still gets the class check — allow-listing "*.example.com" must
	// not become a way to reach 169.254.169.254.
	if isIPLiteral(host) {
		ip, err := netip.ParseAddr(canonicalHost(host))
		if err != nil {
			return netip.Addr{}, &DenyError{Verdict: Verdict{
				Code: CodeDeniedAddress, Reason: fmt.Sprintf("%s is not a usable address", host),
			}}
		}
		ip = ip.Unmap()
		if d.policy.AllowsLiteral(host, port) {
			return ip, nil
		}
		if deny := d.checkAddr(ip, host, port); deny != nil {
			return netip.Addr{}, deny
		}
		return ip, nil
	}

	addrs, err := d.lookup(ctx, host)
	if err != nil {
		// A name that does not resolve is not a policy denial; the caller
		// reports it as a connection failure so the model can tell "DNS is
		// broken" from "policy said no".
		return netip.Addr{}, &DenyError{
			Verdict: Verdict{Code: CodeDeniedAddress, Reason: fmt.Sprintf("%s did not resolve", host)},
			Lookup:  err,
		}
	}
	if len(addrs) == 0 {
		return netip.Addr{}, &DenyError{Verdict: Verdict{
			Code: CodeDeniedAddress, Reason: fmt.Sprintf("%s resolved to no addresses", host),
		}}
	}
	var firstRefusal string
	for _, a := range addrs {
		a = a.Unmap()
		if deny := d.checkAddr(a, host, port); deny != nil {
			if firstRefusal == "" {
				firstRefusal = deny.Verdict.Reason
			}
			continue
		}
		return a, nil
	}
	return netip.Addr{}, &DenyError{Verdict: Verdict{
		Code:   CodeDeniedAddress,
		Host:   host,
		Port:   port,
		Reason: firstRefusal,
	}}
}

// checkAddr applies the class checks and the operator's CIDR list.
func (d *dialer) checkAddr(ip netip.Addr, host string, port int) *DenyError {
	deny := func(why string) *DenyError {
		return &DenyError{Verdict: Verdict{
			Code:    CodeDeniedAddress,
			Host:    host,
			Port:    port,
			Reason:  fmt.Sprintf("%s resolves to %s, which egress does not reach", host, why),
			Address: ip.String(),
		}}
	}
	if class := blockedClass(ip); class != "" {
		return deny(class)
	}
	if rule, ok := d.policy.DeniedAddressReason(ip); ok {
		return deny("a denied address range (" + rule + ")")
	}
	if d.isOwnAddr(ip) {
		return deny("this host's own address")
	}
	return nil
}

// isOwnAddr reports whether ip is assigned to one of this machine's
// interfaces. Cached for ownAddrCacheTTL.
func (d *dialer) isOwnAddr(ip netip.Addr) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ownAddrs == nil || time.Since(d.ownAt) > ownAddrCacheTTL {
		d.ownAddrs = currentInterfaceAddrs()
		d.ownAt = time.Now()
	}
	_, ok := d.ownAddrs[ip.Unmap()]
	return ok
}

func currentInterfaceAddrs() map[netip.Addr]struct{} {
	out := make(map[netip.Addr]struct{}, 8)
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				if ip, ok := netip.AddrFromSlice(v.IP); ok {
					out[ip.Unmap()] = struct{}{}
				}
			case *net.IPAddr:
				if ip, ok := netip.AddrFromSlice(v.IP); ok {
					out[ip.Unmap()] = struct{}{}
				}
			}
		}
	}
	return out
}

// DenyError is a refusal from the address check, carried as an error so the
// dial path stays a normal Go dial. The proxy type-asserts it to choose the
// response code and the reason it shows the client.
type DenyError struct {
	Verdict Verdict
	// Lookup carries the resolver's error when there was one, so a caller
	// can log DNS failure separately from a policy refusal.
	Lookup error
}

func (e *DenyError) Error() string {
	if e.Verdict.Reason != "" {
		return e.Verdict.Reason
	}
	return "connection refused by egress policy"
}

// asDenyError extracts a *DenyError from an error chain.
func asDenyError(err error) (*DenyError, bool) {
	for err != nil {
		if d, ok := err.(*DenyError); ok {
			return d, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
