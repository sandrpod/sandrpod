// Copyright 2026 SandrPod Contributors

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePoderID_Explicit(t *testing.T) {
	id, err := resolvePoderID("my-fixed-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "my-fixed-id" {
		t.Fatalf("expected my-fixed-id, got %s", id)
	}
}

func TestResolvePoderID_Persisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PODER_DATA_DIR", dir)

	// Pre-write an ID file.
	if err := os.WriteFile(filepath.Join(dir, "poder-id"), []byte("poder-persisted-abc\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	id, err := resolvePoderID("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "poder-persisted-abc" {
		t.Fatalf("expected poder-persisted-abc, got %s", id)
	}
}

func TestResolvePoderID_Generated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PODER_DATA_DIR", dir)

	id, err := resolvePoderID("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(id, "poder-") {
		t.Fatalf("generated ID should start with 'poder-', got %s", id)
	}
	if len(id) < 10 {
		t.Fatalf("generated ID too short: %s", id)
	}

	// Second call must return the same ID (persisted).
	id2, err := resolvePoderID("")
	if err != nil {
		t.Fatalf("second call unexpected error: %v", err)
	}
	if id != id2 {
		t.Fatalf("ID not stable across calls: first=%s second=%s", id, id2)
	}
}

func TestResolvePoderID_ExplicitSkipsPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PODER_DATA_DIR", dir)

	// Persist a different ID.
	if err := os.WriteFile(filepath.Join(dir, "poder-id"), []byte("poder-old\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Explicit flag should win over persisted.
	id, err := resolvePoderID("poder-explicit")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "poder-explicit" {
		t.Fatalf("expected poder-explicit, got %s", id)
	}
}
