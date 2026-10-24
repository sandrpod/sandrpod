// Copyright 2026 SandrPod
// Policy tests: the matcher is the part an operator's security depends on, so
// it is tested entry-shape by entry-shape rather than through the proxy.

package egress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, p Policy) *Compiled {
	t.Helper()
	c, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func TestDecide_AllowDenyPrecedence(t *testing.T) {
	c := mustCompile(t, Policy{
		AllowedDomains: []string{"*.example.com", "api.test:8443"},
		DeniedDomains:  []string{"evil.example.com"},
		DeniedDomainReasons: map[string]string{
			"evil.example.com": "use mirror.corp instead",
		},
	})

	cases := []struct {
		host string
		port int
		want bool
		code string
	}{
		{"api.example.com", 443, true, CodeAllowed},
		{"a.b.example.com", 443, true, CodeAllowed},
		{"example.com", 443, false, CodeNotAllowed}, // `*.` does not cover the apex
		{"api.test", 8443, true, CodeAllowed},
		{"api.test", 443, false, CodeNotAllowed}, // port is pinned
		{"evil.example.com", 443, false, CodeDeniedDomain},
		{"other.test", 443, false, CodeNotAllowed},
	}
	for _, tc := range cases {
		v := c.Decide(tc.host, tc.port)
		if v.Allow != tc.want || v.Code != tc.code {
			t.Errorf("Decide(%q:%d) = allow=%v code=%s, want allow=%v code=%s",
				tc.host, tc.port, v.Allow, v.Code, tc.want, tc.code)
		}
	}

	v := c.Decide("evil.example.com", 443)
	if v.Reason != "use mirror.corp instead" {
		t.Errorf("denied_domain_reasons not applied, got %q", v.Reason)
	}
	if v.Rule != "evil.example.com" {
		t.Errorf("verdict should name the deciding rule, got %q", v.Rule)
	}
}

// A denial must never be rescued by a broader allow that also matches.
func TestDecide_DenyBeatsBroaderAllow(t *testing.T) {
	c := mustCompile(t, Policy{
		AllowedDomains: []string{"*"},
		DeniedDomains:  []string{"metadata.internal"},
	})
	if v := c.Decide("metadata.internal", 80); v.Allow {
		t.Fatalf("a denied host matched by `*` must stay denied, got %+v", v)
	}
}

func TestDecide_EmptyAllowListDeniesEverything(t *testing.T) {
	c := mustCompile(t, Policy{})
	if v := c.Decide("example.com", 443); v.Allow {
		t.Fatalf("empty allow list must deny, got %+v", v)
	}
	if !strings.Contains(c.Decide("example.com", 443).Reason, "not on the egress allow list") {
		t.Errorf("reason should say what to do: %q", c.Decide("example.com", 443).Reason)
	}
}

func TestCompileRule_Shapes(t *testing.T) {
	ok := []struct {
		entry    string
		host     string
		port     int
		wildcard bool
		isIP     bool
	}{
		{"example.com", "example.com", 0, false, false},
		{"Example.COM", "example.com", 0, false, false},
		{"*.example.com", ".example.com", 0, true, false},
		{"api.example.com:8443", "api.example.com", 8443, false, false},
		{"127.0.0.1:3000", "127.0.0.1", 3000, false, true},
		{"[::1]:8080", "::1", 8080, false, true},
		{"[2001:db8::1]", "2001:db8::1", 0, false, true},
		{"*", "*", 0, true, false},
		{"my_host.internal", "my_host.internal", 0, false, false},
	}
	for _, tc := range ok {
		r, err := compileRule(tc.entry)
		if err != nil {
			t.Fatalf("compileRule(%q): %v", tc.entry, err)
		}
		if r.Host != tc.host || r.Port != tc.port || r.Wildcard != tc.wildcard || r.IsIP != tc.isIP {
			t.Errorf("compileRule(%q) = %+v, want host=%q port=%d wildcard=%v ip=%v",
				tc.entry, r, tc.host, tc.port, tc.wildcard, tc.isIP)
		}
	}

	bad := []string{
		"",                   // empty
		"http://example.com", // a URL, not an entry
		"example.com/path",   // a path
		"exa mple.com",       // whitespace inside
		"*.",                 // wildcard with no domain
		"api.*.example.com",  // `*` only as a leading label
		"example.com:99999",  // port out of range
		"2001:db8::1:443",    // unbracketed IPv6 + port is ambiguous
		"[::1",               // unclosed bracket
	}
	for _, entry := range bad {
		if _, err := compileRule(entry); err == nil {
			t.Errorf("compileRule(%q) should have failed", entry)
		}
	}
}

// A policy file with a typo'd key must be an error: silently ignoring it
// yields an empty allow-list that reads like "deny everything" at runtime.
func TestLoadPolicy_RejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "egress.json")
	body := `{"allowed_domain":["example.com"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path); err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

func TestLoadPolicy_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "egress.json")
	body := `{
	  "allowed_domains": ["api.github.com", "*.npmjs.org:443"],
	  "denied_domains": ["*.internal"],
	  "denied_domain_reasons": {"*.internal": "internal hosts are not reachable from here"},
	  "denied_resolved_addresses": ["10.0.0.0/8"]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if v := c.Decide("api.github.com", 443); !v.Allow {
		t.Errorf("api.github.com should be allowed: %+v", v)
	}
	if v := c.Decide("x.internal", 443); v.Allow || v.Reason != "internal hosts are not reachable from here" {
		t.Errorf("denied_domain_reasons should apply: %+v", v)
	}
	if _, ok := c.DeniedAddressReason(mustAddr(t, "10.1.2.3")); !ok {
		t.Error("10.0.0.0/8 should be in the denied address list")
	}
}

func TestAllowsLiteral_OnlyForIPEntries(t *testing.T) {
	c := mustCompile(t, Policy{AllowedDomains: []string{"127.0.0.1:3000", "api.example.com"}})
	if !c.AllowsLiteral("127.0.0.1", 3000) {
		t.Error("an explicit IP literal entry should count as a literal allow")
	}
	if c.AllowsLiteral("127.0.0.1", 3001) {
		t.Error("the literal allow is port-pinned")
	}
	if c.AllowsLiteral("api.example.com", 443) {
		t.Error("a hostname entry is not a literal allow")
	}
}
