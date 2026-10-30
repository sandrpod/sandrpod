package sandpod

import (
	"testing"
	"time"
)

// One caller provisions; a second one arriving while the first is in flight waits.
// This is the whole point: two creates seconds apart must not buy two machines.
func TestProvisionBook_SecondCallerWaits(t *testing.T) {
	b := &provisionBook{seen: map[string]time.Time{}}
	key := provisioningKey("aliyun", "cn-hongkong")

	if !b.reserve(key) {
		t.Fatal("the first caller must be the one that provisions")
	}
	if b.reserve(key) {
		t.Fatal("a second caller reserved while the first was in flight")
	}
	if !b.inFlight(key) {
		t.Fatal("the reservation is not visible as in flight")
	}

	b.release(key)
	if !b.reserve(key) {
		t.Fatal("after the first finished, the next caller must be able to provision")
	}
}

// A reservation expires on its own: a control plane killed mid-provision must not
// block every later create for that provider and region.
func TestProvisionBook_Expires(t *testing.T) {
	b := &provisionBook{seen: map[string]time.Time{}}
	key := provisioningKey("aws", "us-east-1")
	b.seen[key] = time.Now().Add(-provisioningTTL - time.Second)

	if b.inFlight(key) {
		t.Fatal("an expired reservation still reads as in flight")
	}
	if !b.reserve(key) {
		t.Fatal("an expired reservation still blocks a new attempt")
	}
}

// Different regions and providers are different machines.
func TestProvisionBook_KeysAreIndependent(t *testing.T) {
	b := &provisionBook{seen: map[string]time.Time{}}
	if !b.reserve(provisioningKey("aliyun", "cn-hongkong")) {
		t.Fatal("first key refused")
	}
	if !b.reserve(provisioningKey("aliyun", "cn-hangzhou")) {
		t.Fatal("a different region was blocked")
	}
	if !b.reserve(provisioningKey("aws", "cn-hongkong")) {
		t.Fatal("a different provider was blocked")
	}
}
