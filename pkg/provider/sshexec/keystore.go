// Copyright 2024 SandrPod
// KeyStore persists per-VM ephemeral SSH private keys so that a control-plane
// restart does not lose the ability to SSH into already-provisioned VMs
// (DigitalOcean, Hetzner, and any other cloud using the shared SSH executor).
//
// Without persistence the signer lives only in the provider's in-memory map:
// after a restart, ExecuteCommand against an existing VM fails because the key
// is gone. With SANDRPOD_SSH_KEY_DIR set, the key is written to
// {dir}/{provider}-{vmID}.pem (0600) and reloaded on demand.

package sshexec

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// KeyStore is a disk-backed store of per-VM SSH signers. A zero/empty dir makes
// every operation a no-op (memory-only, the legacy behavior).
type KeyStore struct {
	dir string
}

// NewKeyStore returns a KeyStore rooted at dir. If dir is empty, the store is
// disabled (Save/Load are no-ops) and callers fall back to their in-memory map.
// The directory is created (0700) when non-empty.
func NewKeyStore(dir string) (*KeyStore, error) {
	if dir == "" {
		return &KeyStore{}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sshexec: create key dir %q: %w", dir, err)
	}
	return &KeyStore{dir: dir}, nil
}

// Enabled reports whether persistence is active.
func (k *KeyStore) Enabled() bool { return k != nil && k.dir != "" }

func (k *KeyStore) path(providerName, vmID string) string {
	return filepath.Join(k.dir, fmt.Sprintf("%s-%s.pem", providerName, sanitize(vmID)))
}

// Save persists the private key behind signer for (provider, vmID). It is a
// no-op when persistence is disabled. Only ed25519 keys (what GenerateEd25519
// produces) are supported.
func (k *KeyStore) Save(providerName, vmID string, priv ed25519.PrivateKey) error {
	if !k.Enabled() {
		return nil
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("sshexec: marshal key: %w", err)
	}
	blk := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	return os.WriteFile(k.path(providerName, vmID), pem.EncodeToMemory(blk), 0o600)
}

// Load reads and parses the persisted signer for (provider, vmID). It returns
// (nil, false, nil) when persistence is disabled or no key file exists.
func (k *KeyStore) Load(providerName, vmID string) (ssh.Signer, bool, error) {
	if !k.Enabled() {
		return nil, false, nil
	}
	data, err := os.ReadFile(k.path(providerName, vmID))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sshexec: read key: %w", err)
	}
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, false, fmt.Errorf("sshexec: no PEM block in key file for %s", vmID)
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, false, fmt.Errorf("sshexec: parse key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, false, fmt.Errorf("sshexec: signer from key: %w", err)
	}
	return signer, true, nil
}

// Delete removes the persisted key for (provider, vmID), if any.
func (k *KeyStore) Delete(providerName, vmID string) {
	if !k.Enabled() {
		return
	}
	_ = os.Remove(k.path(providerName, vmID))
}

// sanitize strips path separators from a VM id so it is safe as a filename.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
