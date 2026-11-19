package server

import (
	"encoding/json"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/e2bcompat"
)

func TestE2BNetworkPolicy(t *testing.T) {
	no := false
	empty, _, err := e2bNetworkPolicy(e2bcompat.NewSandbox{AllowInternetAccess: &no})
	if err != nil || empty == nil || len(empty.AllowedDomains) != 0 {
		t.Fatalf("allow_internet_access=false: got %+v err=%v, want empty policy", empty, err)
	}
	allow, _, err := e2bNetworkPolicy(e2bcompat.NewSandbox{Network: &e2bcompat.E2BNetwork{
		AllowOut: []string{"api.example.com"},
	}})
	if err != nil || allow == nil || len(allow.AllowedDomains) != 1 || allow.AllowedDomains[0] != "api.example.com" {
		t.Fatalf("allow_out: got %+v err=%v", allow, err)
	}
	deny, _, err := e2bNetworkPolicy(e2bcompat.NewSandbox{Network: &e2bcompat.E2BNetwork{
		DenyOut: []string{"blocked.example.com"},
	}})
	if err != nil || deny == nil || len(deny.AllowedDomains) != 1 || deny.AllowedDomains[0] != "*" {
		t.Fatalf("deny_out: got %+v err=%v", deny, err)
	}
	if _, _, err := e2bNetworkPolicy(e2bcompat.NewSandbox{Network: &e2bcompat.E2BNetwork{
		Rules: map[string]json.RawMessage{"example.com": json.RawMessage(`[]`)},
	}}); err == nil {
		t.Fatal("per-method rules must be refused")
	}
	if p, _, err := e2bNetworkPolicy(e2bcompat.NewSandbox{}); err != nil || p != nil {
		t.Fatalf("no policy: got %+v err=%v", p, err)
	}

	noPublic := false
	p, private, err := e2bNetworkPolicy(e2bcompat.NewSandbox{Network: &e2bcompat.E2BNetwork{
		AllowOut:           []string{"example.com"},
		AllowPublicTraffic: &noPublic,
	}})
	if err != nil || p == nil || !private {
		t.Fatalf("allowPublicTraffic=false: policy=%+v private=%v err=%v", p, private, err)
	}
}
