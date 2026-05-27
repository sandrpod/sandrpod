package mcpbridge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_minimal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{
	  "mcpServers": {
	    "github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"]}
	  }
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	gh, ok := cfg.McpServers["github"]
	if !ok {
		t.Fatalf("github entry missing")
	}
	if gh.Command != "npx" {
		t.Errorf("command = %q, want npx", gh.Command)
	}
	if !gh.IsEnabled() {
		t.Errorf("default IsEnabled should be true")
	}
	if got := gh.AliasOr("github"); got != "github" {
		t.Errorf("AliasOr fallback = %q", got)
	}
}

func TestLoadConfig_sandrpodExtensions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	body := `{
	  "mcpServers": {
	    "jira": {
	      "command": "python",
	      "args": ["-m", "mcp_server_jira"],
	      "sandrpod": {
	        "enabled": false,
	        "alias": "jr",
	        "tool_denylist": ["delete_issue"]
	      }
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.McpServers["jira"]
	if j.IsEnabled() {
		t.Errorf("IsEnabled should be false when explicitly disabled")
	}
	if j.AliasOr("jira") != "jr" {
		t.Errorf("alias override not applied")
	}
}

func TestLoadConfig_missingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestSortedKeys_stable(t *testing.T) {
	cfg := &Config{McpServers: map[string]ServerConfig{
		"zeta": {Command: "z"},
		"alpha": {Command: "a"},
		"mid": {Command: "m"},
	}}
	got := cfg.SortedKeys()
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("len=%d", len(got))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: %s vs %s", i, got[i], want[i])
		}
	}
}
