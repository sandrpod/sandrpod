// Copyright 2026 SandrPod
// Guard tests: what an allowed *name* may resolve to.
//
// The resolver is injected, so these are deterministic and never touch DNS —
// which also lets them assert the address that was actually dialled.

package egress

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a.Unmap()
}

// fixedLookup returns the same addresses for every name.
func fixedLookup(addrs ...string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			ip, err := netip.ParseAddr(a)
			if err != nil {
				return nil, err
			}
			out = append(out, ip)
		}
		return out, nil
	}
}

func TestVet_DeniedAddressClasses(t *testing.T) {
	c := mustCompile(t, Policy{AllowedDomains: []string{"*.example.com"}})

	cases := []struct {
		name string
		addr string
		want string // substring of the reason
	}{
		{"loopback", "127.0.0.1", "loopback"},
		{"loopback6", "::1", "loopback"},
		{"link-local", "169.254.169.254", "link-local"},
		{"metadata aliyun", "100.100.100.200", "metadata"},
		{"metadata azure", "168.63.129.16", "metadata"},
		{"unspecified", "0.0.0.0", "unspecified"},
		{"multicast", "224.0.0.1", "multicast"},
	}
	for _, tc := range cases {
		d := newDialer(c, time.Second)
		d.lookup = fixedLookup(tc.addr)
		_, deny := d.vet(context.Background(), "api.example.com", 443)
		if deny == nil {
			t.Errorf("%s (%s): expected a refusal", tc.name, tc.addr)
			continue
		}
		if !strings.Contains(deny.Verdict.Reason, tc.want) {
			t.Errorf("%s: reason %q should mention %q", tc.name, deny.Verdict.Reason, tc.want)
		}
		if deny.Verdict.Code != CodeDeniedAddress {
			t.Errorf("%s: code = %q, want %q", tc.name, deny.Verdict.Code, CodeDeniedAddress)
		}
	}
}

// One bad address among several must not poison the name: the surviving
// address is dialled. This is what makes a load-balanced host that lists an
// internal address alongside a public one still usable.
func TestVet_UsesTheSurvivingAddress(t *testing.T) {
	c := mustCompile(t, Policy{AllowedDomains: []string{"*.example.com"}})
	d := newDialer(c, time.Second)
	d.lookup = fixedLookup("127.0.0.1", "93.184.216.34")

	ip, deny := d.vet(context.Background(), "api.example.com", 443)
	if deny != nil {
		t.Fatalf("expected the public address to survive, got %v", deny)
	}
	if ip.String() != "93.184.216.34" {
		t.Fatalf("dialled %s, want the vetted public address", ip)
	}
}

func TestVet_DeniedResolvedAddressCIDR(t *testing.T) {
	c := mustCompile(t, Policy{
		AllowedDomains:          []string{"*.example.com"},
		DeniedResolvedAddresses: []string{"10.0.0.0/8"},
	})
	d := newDialer(c, time.Second)
	d.lookup = fixedLookup("10.1.2.3")

	_, deny := d.vet(context.Background(), "api.example.com", 443)
	if deny == nil {
		t.Fatal("10.1.2.3 is in denied_resolved_addresses and must be refused")
	}
	if !strings.Contains(deny.Verdict.Reason, "10.0.0.0/8") {
		t.Errorf("reason should name the rule: %q", deny.Verdict.Reason)
	}
}

// Naming an IP literal in the allow list is an explicit decision, so the
// class check does not second-guess it. Fetching the same address by a *name*
// that resolves to it is a different act and stays refused — that is the
// whole point of the guard.
func TestVet_LiteralAllowVsName(t *testing.T) {
	c := mustCompile(t, Policy{AllowedDomains: []string{"127.0.0.1:8123", "localhost:8123"}})
	d := newDialer(c, time.Second)
	d.lookup = fixedLookup("127.0.0.1")

	if ip, deny := d.vet(context.Background(), "127.0.0.1", 8123); deny != nil || ip.String() != "127.0.0.1" {
		t.Fatalf("an explicit literal allow must pass: ip=%v deny=%v", ip, deny)
	}

	ip, deny := d.vet(context.Background(), "localhost", 8123)
	if deny == nil {
		t.Fatalf("a name resolving to loopback must be refused, got %v", ip)
	}
	if !strings.Contains(deny.Verdict.Reason, "loopback") {
		t.Errorf("reason should name the class: %q", deny.Verdict.Reason)
	}
}

// An allowed name pointing at one of this host's own interface addresses is
// an internal service, not the internet.
func TestVet_OwnInterfaceAddress(t *testing.T) {
	c := mustCompile(t, Policy{AllowedDomains: []string{"*.example.com"}})
	d := newDialer(c, time.Second)
	own := firstNonLoopbackInterfaceAddr(t)
	if own == "" {
		t.Skip("no non-loopback interface to test with")
	}
	d.lookup = fixedLookup(own)

	_, deny := d.vet(context.Background(), "api.example.com", 443)
	if deny == nil {
		t.Fatalf("this host's own address %s must be refused", own)
	}
	if !strings.Contains(deny.Verdict.Reason, "own address") {
		t.Errorf("reason should name the class: %q", deny.Verdict.Reason)
	}
}

func TestVet_UnresolvableNameIsNotAPolicyDenial(t *testing.T) {
	c := mustCompile(t, Policy{AllowedDomains: []string{"*.example.com"}})
	d := newDialer(c, time.Second)
	d.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "api.example.com"}
	}
	_, deny := d.vet(context.Background(), "api.example.com", 443)
	if deny == nil {
		t.Fatal("expected a refusal")
	}
	if deny.Lookup == nil {
		t.Error("the DNS error should be carried so the caller can distinguish it from policy")
	}
	if !strings.Contains(deny.Verdict.Reason, "did not resolve") {
		t.Errorf("reason should say the name did not resolve: %q", deny.Verdict.Reason)
	}
}

func firstNonLoopbackInterfaceAddr(t *testing.T) string {
	t.Helper()
	addrs := currentInterfaceAddrs()
	for ip := range addrs {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
			continue
		}
		return ip.String()
	}
	return ""
}
