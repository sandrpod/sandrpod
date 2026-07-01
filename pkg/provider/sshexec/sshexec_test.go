package sshexec

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateEd25519(t *testing.T) {
	signer, authKey, err := GenerateEd25519("sandrpod")
	if err != nil {
		t.Fatalf("GenerateEd25519: %v", err)
	}
	if signer == nil {
		t.Fatal("nil signer")
	}
	if !strings.HasPrefix(authKey, "ssh-ed25519 ") {
		t.Errorf("authorized key not ed25519: %q", authKey)
	}
	if !strings.HasSuffix(authKey, " sandrpod") {
		t.Errorf("authorized key missing comment: %q", authKey)
	}
	// The public key must be parseable and match the signer's.
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authKey))
	if err != nil {
		t.Fatalf("authorized key not parseable: %v", err)
	}
	if pub.Type() != signer.PublicKey().Type() {
		t.Errorf("key type mismatch: %s vs %s", pub.Type(), signer.PublicKey().Type())
	}

	// Two calls must produce distinct ephemeral keys.
	_, authKey2, _ := GenerateEd25519("sandrpod")
	if authKey == authKey2 {
		t.Error("expected distinct ephemeral keys")
	}
}

func TestCloudInitRootKey(t *testing.T) {
	ci := CloudInitRootKey("ssh-ed25519 AAAAC3xyz sandrpod")
	if !strings.HasPrefix(ci, "#cloud-config") {
		t.Errorf("must start with #cloud-config, got %q", ci[:20])
	}
	if !strings.Contains(ci, "/root/.ssh/authorized_keys") {
		t.Error("must write to root authorized_keys")
	}
	if !strings.Contains(ci, "ssh-ed25519 AAAAC3xyz sandrpod") {
		t.Error("must embed the authorized key")
	}
}
