package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// --- Issue 2: sensitive-tool re-prompt --------------------------------------

func TestIsSensitiveTool(t *testing.T) {
	for _, want := range []string{
		"delete_repo", "delete_entities",
		"remove_user", "drop_table", "purge_cache",
		"destroy_record", "wipe_data",
		"send_email", "send_message", "send_dm",
		"publish_post", "post_tweet",
		"transfer_funds", "pay_invoice", "charge_card",
		"merge_pr", "revoke_token", "reset_password", "unsubscribe_all",
	} {
		if !isSensitiveTool(want) {
			t.Errorf("expected %q to be sensitive", want)
		}
	}
	for _, no := range []string{
		"list_issues", "get_user", "read_graph",
		"create_entities", "add_observations", "query_docs",
		"resolve_library_id", "search_nodes",
	} {
		if isSensitiveTool(no) {
			t.Errorf("expected %q to be NOT sensitive", no)
		}
	}
}

func TestSensitivePatterns_EnvOverride(t *testing.T) {
	t.Setenv("SANDRPOD_MCP_SENSITIVE_PATTERNS_OVERRIDE", "fubar,nuke")
	// Override REPLACES, so previously-sensitive names should no longer be.
	if isSensitiveTool("delete_repo") {
		t.Errorf("override should drop default patterns")
	}
	if !isSensitiveTool("nuke_everything") {
		t.Errorf("override pattern not honored")
	}
}

func TestSensitivePatterns_EnvExtend(t *testing.T) {
	t.Setenv("SANDRPOD_MCP_SENSITIVE_PATTERNS", "fubar,custom_op")
	// Extend keeps defaults.
	if !isSensitiveTool("delete_repo") {
		t.Errorf("default patterns must remain when extending")
	}
	if !isSensitiveTool("custom_op_now") {
		t.Errorf("extended pattern not honored")
	}
}

// scriptedNotifier returns canned responses; counts how many prompts it saw.
type scriptedNotifier struct {
	responses []permission.PromptResponse
	calls     int
}

func (s *scriptedNotifier) Ask(_ context.Context, _ permission.Request) (permission.PromptResponse, error) {
	resp := s.responses[s.calls%len(s.responses)]
	s.calls++
	return resp, nil
}

func TestPermissionAdapter_NormalToolCachesAfterPermanent(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "grants.json")
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptAllowPermanent}}
	a, err := newMCPPermissionAdapter(n, store)
	if err != nil {
		t.Fatal(err)
	}

	// First call: prompts, gets allow_permanent, persists.
	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "gh", Tool: "list_issues"}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatalf("first call: want Allow")
	}
	// Second call: no prompt, served from cache.
	_, _ = a.Check(context.Background(), evt)
	if n.calls != 1 {
		t.Errorf("non-sensitive tool should be cached after permanent grant; prompts=%d", n.calls)
	}

	// Verify it was persisted to disk.
	body, _ := os.ReadFile(store)
	if !strings.Contains(string(body), `"gh:list_issues": true`) {
		t.Errorf("expected gh:list_issues in grants file, got: %s", body)
	}
}

func TestPermissionAdapter_SensitiveToolAlwaysPrompts(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "grants.json")
	// Even when the user clicks "allow permanent", we want subsequent
	// calls to prompt again. Verify that.
	n := &scriptedNotifier{responses: []permission.PromptResponse{
		permission.PromptAllowPermanent,
		permission.PromptAllowOnce,
		permission.PromptDeny,
	}}
	a, err := newMCPPermissionAdapter(n, store)
	if err != nil {
		t.Fatal(err)
	}
	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "gh", Tool: "delete_repo"}

	// Call 1: allow_permanent → allowed but NOT persisted.
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatalf("call 1: want Allow")
	}
	// Call 2: prompts again, allow_once → allowed.
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatalf("call 2: want Allow")
	}
	// Call 3: prompts again, deny → denied.
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionDeny {
		t.Fatalf("call 3: want Deny")
	}
	if n.calls != 3 {
		t.Errorf("sensitive tool must prompt every time; prompts=%d (want 3)", n.calls)
	}

	// And the grants file must NOT contain the sensitive tool.
	body, _ := os.ReadFile(store)
	if strings.Contains(string(body), "delete_repo") {
		t.Errorf("sensitive tool should not be persisted; file has: %s", body)
	}
}
