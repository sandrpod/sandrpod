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
	a := newMCPPermissionAdapter(n, store)

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
	a := newMCPPermissionAdapter(n, store)
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

func TestPermissionAdapter_SessionGrantCachedUntilRestart(t *testing.T) {
	store := filepath.Join(t.TempDir(), "grants.json")
	n := &scriptedNotifier{responses: []permission.PromptResponse{
		permission.PromptAllowSession,
		permission.PromptDeny, // only reachable after the "restart"
	}}
	a := newMCPPermissionAdapter(n, store)
	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "browser", Tool: "navigate"}

	// Call 1 prompts (allow_session); call 2 is served from the session cache.
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatal("call 1: want Allow")
	}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatal("call 2: want Allow from session cache")
	}
	if n.calls != 1 {
		t.Errorf("session grant must silence later calls; prompts=%d (want 1)", n.calls)
	}
	// Session grants are memory-only — never persisted.
	if body, err := os.ReadFile(store); err == nil && strings.Contains(string(body), "navigate") {
		t.Errorf("session grant must not be persisted; file has: %s", body)
	}

	// "Restart" the agent: a fresh adapter over the same store prompts again.
	b := newMCPPermissionAdapter(n, store)
	if d, _ := b.Check(context.Background(), evt); d != mcpbridge.DecisionDeny {
		t.Fatal("after restart: session grant must be gone (scripted deny)")
	}
	if n.calls != 2 {
		t.Errorf("fresh adapter should have prompted once more; prompts=%d (want 2)", n.calls)
	}
}

func TestPermissionAdapter_SessionGrantCoversSpawnAndRestart(t *testing.T) {
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptAllowSession}}
	a := newMCPPermissionAdapter(n, filepath.Join(t.TempDir(), "grants.json"))
	spawn := mcpbridge.PermissionEvent{Source: "mcp.spawn", Server: "browser", Command: "node"}
	restart := mcpbridge.PermissionEvent{Source: "mcp.restart", Server: "browser"}

	if d, _ := a.Check(context.Background(), spawn); d != mcpbridge.DecisionAllow {
		t.Fatal("spawn: want Allow")
	}
	// Restart shares the server-level session grant — no second prompt.
	if d, _ := a.Check(context.Background(), restart); d != mcpbridge.DecisionAllow {
		t.Fatal("restart: want Allow from session cache")
	}
	if n.calls != 1 {
		t.Errorf("server-level session grant should cover restart; prompts=%d (want 1)", n.calls)
	}
}

func TestPermissionAdapter_SensitiveToolNeverSessionCached(t *testing.T) {
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptAllowSession}}
	a := newMCPPermissionAdapter(n, filepath.Join(t.TempDir(), "grants.json"))
	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "gh", Tool: "send_message"}

	for i := 1; i <= 2; i++ {
		if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
			t.Fatalf("call %d: want Allow", i)
		}
	}
	if n.calls != 2 {
		t.Errorf("sensitive tool must prompt every time even after allow_session; prompts=%d (want 2)", n.calls)
	}
}

func TestPermissionAdapter_ServerWildcardGrant(t *testing.T) {
	store := filepath.Join(t.TempDir(), "grants.json")
	// Operator pre-approved every non-sensitive browser tool via the file.
	seed := `{"version":1,"servers":{},"tools":{"browser:*":true}}`
	if err := os.WriteFile(store, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptAllowOnce}}
	a := newMCPPermissionAdapter(n, store)

	// Any non-sensitive tool on the wildcarded server: silent allow.
	for _, tool := range []string{"navigate", "screenshot", "click"} {
		evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "browser", Tool: tool}
		if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
			t.Fatalf("browser:%s: want Allow via wildcard", tool)
		}
	}
	if n.calls != 0 {
		t.Errorf("wildcard must not prompt for non-sensitive tools; prompts=%d", n.calls)
	}

	// Sensitive tool on the same server: the wildcard must NOT cover it.
	del := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "browser", Tool: "delete_history"}
	if d, _ := a.Check(context.Background(), del); d != mcpbridge.DecisionAllow {
		t.Fatal("browser:delete_history: want Allow (scripted allow_once)")
	}
	if n.calls != 1 {
		t.Errorf("sensitive tool must still prompt despite wildcard; prompts=%d (want 1)", n.calls)
	}

	// A different server is out of the wildcard's scope.
	other := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "gh", Tool: "list_issues"}
	if _, err := a.Check(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if n.calls != 2 {
		t.Errorf("wildcard is per-server; gh should have prompted; prompts=%d (want 2)", n.calls)
	}
}

func TestPermissionAdapter_HandEditTakesEffectWithoutRestart(t *testing.T) {
	store := filepath.Join(t.TempDir(), "grants.json")
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptDeny}}
	a := newMCPPermissionAdapter(n, store) // file absent at startup

	// Operator hand-writes a wildcard while the agent is running.
	if err := os.WriteFile(store, []byte(`{"version":1,"tools":{"browser:*":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "browser", Tool: "find"}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatal("hand-edited wildcard should allow without a restart")
	}
	if n.calls != 0 {
		t.Errorf("no prompt expected — reload-on-miss should have picked up the edit; prompts=%d", n.calls)
	}
}

func TestPermissionAdapter_DeletingGrantsFileRevokes(t *testing.T) {
	store := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(store, []byte(`{"version":1,"tools":{"browser:*":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptDeny}}
	a := newMCPPermissionAdapter(n, store)

	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "browser", Tool: "find"}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatal("wildcard present at startup should allow")
	}
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionDeny {
		t.Fatal("deleting the grants file should revoke persistent grants")
	}
	if n.calls != 1 {
		t.Errorf("expected exactly one prompt after revocation; prompts=%d", n.calls)
	}
}

func TestPermissionAdapter_CorruptFileDegradesToPromptNotAllowAll(t *testing.T) {
	store := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(store, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptDeny}}
	a := newMCPPermissionAdapter(n, store) // must not fail construction

	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "gh", Tool: "list_issues"}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionDeny {
		t.Fatal("corrupt grants file must degrade to prompting (deny here), never allow-all")
	}
	if n.calls != 1 {
		t.Errorf("expected the call to prompt; prompts=%d", n.calls)
	}
}

func TestPermissionAdapter_CorruptReloadKeepsLastGoodGrants(t *testing.T) {
	store := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(store, []byte(`{"version":1,"tools":{"browser:*":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptDeny}}
	a := newMCPPermissionAdapter(n, store)

	// File turns to garbage while running (e.g. interrupted manual edit).
	if err := os.WriteFile(store, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Covered tool: still allowed from the last good in-memory state.
	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "browser", Tool: "find"}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatal("corrupt reload should keep the last good grants")
	}
	if n.calls != 0 {
		t.Errorf("covered tool should not prompt; prompts=%d", n.calls)
	}
}

func TestPermissionAdapter_PermanentGrantPreservesHandEdit(t *testing.T) {
	store := filepath.Join(t.TempDir(), "grants.json")
	// Operator wildcard already on disk; adapter has NOT loaded it (file
	// written after construction) — the flush-merge must not clobber it.
	n := &scriptedNotifier{responses: []permission.PromptResponse{permission.PromptAllowPermanent}}
	a := newMCPPermissionAdapter(n, store)
	if err := os.WriteFile(store, []byte(`{"version":1,"tools":{"browser:*":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A different server's tool prompts → permanent → flush.
	evt := mcpbridge.PermissionEvent{Source: "mcp.call", Server: "gh", Tool: "list_issues"}
	if d, _ := a.Check(context.Background(), evt); d != mcpbridge.DecisionAllow {
		t.Fatal("want Allow")
	}
	body, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"browser:*": true`, `"gh:list_issues": true`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("flush clobbered or missed %s; file: %s", key, body)
		}
	}
}
