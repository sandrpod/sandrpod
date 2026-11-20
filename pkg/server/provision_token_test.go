package server

import (
	"strings"
	"testing"
	"time"
)

const provisionSecret = "platform-secret"

func TestProvisionToken_RoundTripsAsScopedWorker(t *testing.T) {
	now := time.Now()
	tok := MintProvisionToken(provisionSecret, "vm-123", "tenant-a", time.Hour, now)
	if !strings.HasPrefix(tok, ProvisionTokenPrefix) {
		t.Fatalf("token %q is not marked with the provision prefix", tok)
	}
	id, ok := verifyProvisionToken(provisionSecret, tok, now.Add(time.Minute))
	if !ok {
		t.Fatal("a freshly minted token did not verify")
	}
	if id.Role != RoleWorker {
		t.Errorf("role = %q, want worker: an administrator key on a provisioned VM is the thing this replaces", id.Role)
	}
	if id.Tenant != "tenant-a" {
		t.Errorf("tenant = %q, want tenant-a: placement and heartbeat both key off it", id.Tenant)
	}
	if id.Name != "provision:vm-123" {
		t.Errorf("name = %q", id.Name)
	}
	if id.IsAdmin() {
		t.Error("a provision token must never be an administrator")
	}
}

func TestProvisionToken_Expires(t *testing.T) {
	now := time.Now()
	tok := MintProvisionToken(provisionSecret, "vm-1", "", time.Minute, now)
	if _, ok := verifyProvisionToken(provisionSecret, tok, now.Add(2*time.Minute)); ok {
		t.Fatal("an expired token verified")
	}
}

func TestProvisionToken_RejectsTampering(t *testing.T) {
	now := time.Now()
	tok := MintProvisionToken(provisionSecret, "vm-1", "tenant-a", time.Hour, now)

	cases := map[string]string{
		"different secret":      MintProvisionToken("other-secret", "vm-1", "tenant-a", time.Hour, now),
		"truncated":             tok[:len(tok)-2],
		"no signature":          strings.SplitN(tok, ".", 2)[0],
		"not a provision token": "e2b_deadbeef",
		"empty":                 "",
	}
	for name, candidate := range cases {
		if _, ok := verifyProvisionToken(provisionSecret, candidate, now); ok {
			t.Errorf("%s: verified, want refusal", name)
		}
	}

	// The payload is signed, so editing the tenant without the secret fails even
	// though the token still parses.
	body := strings.TrimPrefix(tok, ProvisionTokenPrefix)
	payload, sig, _ := strings.Cut(body, ".")
	if _, ok := verifyProvisionToken(provisionSecret, ProvisionTokenPrefix+payload+"x."+sig, now); ok {
		t.Error("a mutated payload verified")
	}
}

func TestProvisionToken_NoSecretMintsNothing(t *testing.T) {
	// Auth disabled: there is no secret, so the scheduler keeps starting workers
	// without a credential instead of inventing one.
	if tok := MintProvisionToken("", "vm-1", "tenant-a", time.Hour, time.Now()); tok != "" {
		t.Fatalf("minted %q with no secret", tok)
	}
	if _, ok := verifyProvisionToken("", "pv1_anything.atall", time.Now()); ok {
		t.Fatal("verified a token with no secret")
	}
}

// The point of the whole file: the token must authenticate through the ordinary
// request path, as a worker, without becoming an administrator.
func TestProvisionToken_ResolvesAsWorkerNotAdmin(t *testing.T) {
	now := time.Now()
	cfg := Config{Token: provisionSecret}
	tok := MintProvisionToken(provisionSecret, "vm-9", "tenant-b", time.Hour, now)

	id, ok := resolveToken(cfg, tok)
	if !ok {
		t.Fatal("resolveToken refused a valid provision token")
	}
	if id.IsAdmin() {
		t.Fatal("a provision token resolved as admin")
	}
	if !id.IsWorker() || id.Tenant != "tenant-b" {
		t.Fatalf("identity = %+v, want worker in tenant-b", id)
	}

	// The platform token itself still resolves as admin — this adds a
	// credential, it does not weaken the operator's.
	if admin, ok := resolveToken(cfg, provisionSecret); !ok || !admin.IsAdmin() {
		t.Fatalf("the platform token stopped being admin: %+v %v", admin, ok)
	}
}
