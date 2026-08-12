package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

func TestMCPAuditAdapterNamesThePrompt(t *testing.T) {
	got := recordAndRead(t, mcpbridge.AuditEvent{
		Source:   "mcp.prompt",
		Decision: mcpbridge.DecisionAllow,
		Server:   "dinvora",
		Prompt:   "summarise",
	})
	if !strings.Contains(got.Caller, "summarise") {
		t.Errorf("caller = %q, want it to name the prompt", got.Caller)
	}
}

// A guard against adding a fourth identifying field to AuditEvent and
// forgetting the branch — which has now happened twice, once for Resource and
// once for Prompt. Every field that identifies *what* was acted on must reach
// the log; reflection notices the next one without anyone remembering to.
func TestMCPAuditAdapterCarriesEveryIdentifyingField(t *testing.T) {
	// Fields on AuditEvent that name the target of the action, as opposed to
	// describing the outcome. Extend deliberately, not to silence a failure.
	identifying := []string{"Tool", "Resource", "Prompt"}

	typ := reflect.TypeOf(mcpbridge.AuditEvent{})
	for _, name := range identifying {
		if _, ok := typ.FieldByName(name); !ok {
			t.Fatalf("AuditEvent has no field %s — update this list", name)
		}
	}

	// Anything string-typed and not in the known-descriptive set is a
	// candidate the list may have missed.
	descriptive := map[string]bool{
		"Source": true, "Server": true, "Decision": true, "ArgsSummary": true,
		"ResultStatus": true, "Reason": true, "SessionID": true, "Caller": true,
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String || descriptive[f.Name] {
			continue
		}
		if !slices.Contains(identifying, f.Name) {
			t.Errorf("AuditEvent.%s is a new string field: either add it to the "+
				"adapter's switch and to `identifying`, or to `descriptive`", f.Name)
		}
	}

	for _, name := range identifying {
		ev := mcpbridge.AuditEvent{Source: "mcp.x", Server: "s"}
		reflect.ValueOf(&ev).Elem().FieldByName(name).SetString("VALUE-" + name)
		got := recordAndRead(t, ev)
		if !strings.Contains(got.Caller, "VALUE-"+name) {
			t.Errorf("AuditEvent.%s never reaches the audit line (caller=%q)", name, got.Caller)
		}
	}
}
