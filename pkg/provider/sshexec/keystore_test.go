package sshexec

import "testing"

func TestKeyStore_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ks, err := NewKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ks.Enabled() {
		t.Fatal("keystore with a dir should be enabled")
	}

	signer, _, priv, err := GenerateEd25519Key("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Save("digitalocean", "vm-123", priv); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, found, err := ks.Load("digitalocean", "vm-123")
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if string(loaded.PublicKey().Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("loaded signer public key does not match original")
	}

	ks.Delete("digitalocean", "vm-123")
	if _, found, _ := ks.Load("digitalocean", "vm-123"); found {
		t.Error("key should be gone after Delete")
	}
}

func TestKeyStore_Disabled(t *testing.T) {
	ks, err := NewKeyStore("")
	if err != nil {
		t.Fatal(err)
	}
	if ks.Enabled() {
		t.Fatal("empty-dir keystore must be disabled")
	}
	_, _, priv, _ := GenerateEd25519Key("x")
	if err := ks.Save("p", "v", priv); err != nil {
		t.Errorf("disabled Save should be a no-op, got %v", err)
	}
	if _, found, err := ks.Load("p", "v"); found || err != nil {
		t.Errorf("disabled Load should return (nil,false,nil), got found=%v err=%v", found, err)
	}
}

func TestKeyStore_LoadMissing(t *testing.T) {
	ks, _ := NewKeyStore(t.TempDir())
	if _, found, err := ks.Load("p", "nope"); found || err != nil {
		t.Errorf("missing key: found=%v err=%v", found, err)
	}
}
