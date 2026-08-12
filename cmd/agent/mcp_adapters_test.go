package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/audit"
	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

// The adapter packs bridge events into the versioned audit schema by hand, so
// a field added to mcpbridge.AuditEvent does not reach the log until someone
// wires it here. That is exactly how the resource URI went missing: Source
// said "mcp.resource", but the line named no resource, leaving an audit trail
// that could tell you a read happened and not what was read.
func recordAndRead(t *testing.T, ev mcpbridge.AuditEvent) audit.Event {
	t.Helper()
	dir := t.TempDir()
	rec, err := audit.NewRecorder(audit.Options{Dir: dir})
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	a := &mcpAuditAdapter{rec: rec}
	a.Record(ev)
	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no audit file written in %s (%v)", dir, err)
	}
	var last audit.Event
	found := false
	for _, f := range entries {
		blob, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(blob)), "\n") {
			if line == "" {
				continue
			}
			var e audit.Event
			if json.Unmarshal([]byte(line), &e) == nil && e.Source != "" {
				last, found = e, true
			}
		}
	}
	if !found {
		t.Fatal("no parseable audit event written")
	}
	return last
}

func TestMCPAuditAdapterNamesTheResource(t *testing.T) {
	got := recordAndRead(t, mcpbridge.AuditEvent{
		Source:       "mcp.resource",
		Decision:     mcpbridge.DecisionAllow,
		Server:       "dinvora",
		Resource:     "ui://dinvora/todo-cards",
		ResultStatus: "ok",
		DurationMs:   4,
	})

	if got.Source != "mcp.resource" {
		t.Errorf("source = %q", got.Source)
	}
	if !strings.Contains(got.Caller, "ui://dinvora/todo-cards") {
		t.Errorf("caller = %q, want it to name the resource read", got.Caller)
	}
	if got.Path != "mcp:dinvora" {
		t.Errorf("path = %q, want the mcp:<server> grouping key", got.Path)
	}
	if !strings.Contains(got.Reason, "status=ok") {
		t.Errorf("reason = %q, want the result status", got.Reason)
	}
}

func TestMCPAuditAdapterNamesTheTool(t *testing.T) {
	got := recordAndRead(t, mcpbridge.AuditEvent{
		Source:   "mcp.call",
		Decision: mcpbridge.DecisionAllow,
		Server:   "dinvora",
		Tool:     "my_todo",
	})
	if !strings.Contains(got.Caller, "my_todo") {
		t.Errorf("caller = %q, want it to name the tool", got.Caller)
	}
	// A tool call must not pick up a resource label, and vice versa — the two
	// branches are mutually exclusive.
	if strings.Contains(got.Caller, "ui://") {
		t.Errorf("caller = %q leaked a resource URI into a tool call", got.Caller)
	}
}

// Args and results may carry customer data; the reason line must never
// include them even truncated.
func TestMCPAuditAdapterRedactsPayloads(t *testing.T) {
	got := recordAndRead(t, mcpbridge.AuditEvent{
		Source:      "mcp.call",
		Decision:    mcpbridge.DecisionAllow,
		Server:      "dinvora",
		Tool:        "my_todo",
		ArgsSummary: "{\"ssn\":\"123-45-6789\"}",
	})
	if strings.Contains(got.Reason, "123-45-6789") || strings.Contains(got.Caller, "123-45-6789") {
		t.Errorf("argsSummary leaked into the audit line: caller=%q reason=%q", got.Caller, got.Reason)
	}
}
