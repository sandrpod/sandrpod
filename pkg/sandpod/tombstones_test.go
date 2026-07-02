package sandpod

import (
	"testing"
	"time"
)

func TestTombstones_AddContains(t *testing.T) {
	ts := NewTombstones(time.Minute)
	if ts.Contains("poder-x") {
		t.Error("empty set must not contain anything")
	}
	ts.Add("poder-x")
	if !ts.Contains("poder-x") {
		t.Error("added id must be contained")
	}
	if ts.Contains("poder-y") {
		t.Error("unrelated id must not be contained")
	}
}

func TestTombstones_Expiry(t *testing.T) {
	ts := NewTombstones(10 * time.Millisecond)
	ts.Add("poder-x")
	if !ts.Contains("poder-x") {
		t.Fatal("must be contained before expiry")
	}
	time.Sleep(20 * time.Millisecond)
	if ts.Contains("poder-x") {
		t.Error("must expire after ttl")
	}
}

func TestTombstones_Concurrent(t *testing.T) {
	ts := NewTombstones(time.Minute)
	done := make(chan struct{})
	go func() {
		for range 1000 {
			ts.Add("a")
		}
		close(done)
	}()
	for range 1000 {
		ts.Contains("a")
	}
	<-done
}
