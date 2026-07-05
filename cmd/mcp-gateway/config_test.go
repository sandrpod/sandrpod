package main

import "testing"

func TestTranslateConfig_NativePassthrough(t *testing.T) {
	cfg, err := translateConfig(`{"mcpServers":{"fs":{"command":"npx","args":["-y","server"]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.McpServers["fs"]
	if !ok || s.Command != "npx" || len(s.Args) != 2 {
		t.Fatalf("native passthrough wrong: %+v", cfg.McpServers)
	}
}

func TestTranslateConfig_E2BCustomServer(t *testing.T) {
	// E2B custom-server shape: installCmd + runCmd → sh -c "install && run".
	cfg, err := translateConfig(`{"weather":{"installCmd":"pip install x","runCmd":"python -m server"}}`)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.McpServers["weather"]
	if !ok || s.Command != "sh" || len(s.Args) != 2 {
		t.Fatalf("custom server not translated: %+v", cfg.McpServers)
	}
	if got := s.Args[1]; got != "pip install x && python -m server" {
		t.Fatalf("shell wrong: %q", got)
	}
}

func TestTranslateConfig_GitHubRepoClones(t *testing.T) {
	cfg, _ := translateConfig(`{"github/owner/repo":{"installCmd":"npm i","runCmd":"node ."}}`)
	s, ok := cfg.McpServers["github_owner_repo"]
	if !ok {
		t.Fatalf("repo key not sanitized/mapped: %+v", cfg.McpServers)
	}
	sh := s.Args[1]
	if !contains(sh, "git clone") || !contains(sh, "https://github.com/owner/repo") || !contains(sh, "npm i && node .") {
		t.Fatalf("repo clone shell wrong: %q", sh)
	}
}

func TestTranslateConfig_CatalogOnlySkipped(t *testing.T) {
	// A catalog entry with only credentials (no runCmd) can't be resolved → skipped.
	cfg, err := translateConfig(`{"notion":{"internalIntegrationToken":"secret"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.McpServers) != 0 {
		t.Fatalf("catalog-only should be skipped, got %+v", cfg.McpServers)
	}
}

func TestTranslateConfig_CatalogExaResolves(t *testing.T) {
	// The documented E2B shape {exa:{apiKey}} → npx exa-mcp-server, EXA_API_KEY.
	cfg, err := translateConfig(`{"exa":{"apiKey":"exa-key-123"}}`)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.McpServers["exa"]
	if !ok {
		t.Fatalf("exa catalog entry not resolved: %+v", cfg.McpServers)
	}
	if s.Command != "npx" || len(s.Args) < 2 || s.Args[len(s.Args)-1] != "exa-mcp-server" {
		t.Fatalf("exa launch wrong: %+v", s)
	}
	if s.Env["EXA_API_KEY"] != "exa-key-123" {
		t.Fatalf("exa credential not mapped to EXA_API_KEY: %+v", s.Env)
	}
}

func TestTranslateConfig_CatalogBrowserbaseMultiCred(t *testing.T) {
	cfg, _ := translateConfig(`{"browserbase":{"apiKey":"a","projectId":"p","geminiApiKey":"g"}}`)
	s, ok := cfg.McpServers["browserbase"]
	if !ok {
		t.Fatalf("browserbase not resolved: %+v", cfg.McpServers)
	}
	want := map[string]string{"BROWSERBASE_API_KEY": "a", "BROWSERBASE_PROJECT_ID": "p", "GEMINI_API_KEY": "g"}
	for k, v := range want {
		if s.Env[k] != v {
			t.Fatalf("browserbase env %s=%q, want %q (env=%+v)", k, s.Env[k], v, s.Env)
		}
	}
}

func TestTranslateConfig_CatalogUnmappedCredFallsBackToUpperSnake(t *testing.T) {
	// exa is seeded but only maps apiKey; an unlisted key must still be injected
	// via the UPPER_SNAKE fallback so a valid-but-unmapped cred is not dropped.
	cfg, _ := translateConfig(`{"exa":{"apiKey":"k","numResults":"x","maxTokens":"y"}}`)
	s := cfg.McpServers["exa"]
	if s.Env["NUM_RESULTS"] != "x" || s.Env["MAX_TOKENS"] != "y" {
		t.Fatalf("unmapped creds not upper-snaked: %+v", s.Env)
	}
}

func TestUpperSnake(t *testing.T) {
	cases := map[string]string{"apiKey": "API_KEY", "geminiApiKey": "GEMINI_API_KEY", "token": "TOKEN", "projectId": "PROJECT_ID"}
	for in, want := range cases {
		if got := upperSnake(in); got != want {
			t.Errorf("upperSnake(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestTranslateConfig_Empty(t *testing.T) {
	cfg, err := translateConfig("")
	if err != nil || cfg.McpServers == nil || len(cfg.McpServers) != 0 {
		t.Fatalf("empty config should give empty map: %+v err=%v", cfg, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
