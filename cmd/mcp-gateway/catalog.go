package main

import (
	"strings"

	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

// catalogServer describes how to launch a Docker-MCP-Catalog server WITHOUT
// Docker — as a plain stdio child (npx/uvx) — plus how to turn E2B's credential
// object into the environment variables that server reads.
//
// Why this exists: E2B's mcp-gateway resolves catalog names (exa, github, …)
// against the Docker MCP Catalog and runs each tool as its own Docker container.
// sandrpod sandboxes are themselves containers with no Docker inside, so we run
// the SAME servers as stdio children via the npx/uvx launchers the toolbox image
// already ships. The tool surface is equivalent; only the launch mechanism
// differs. See docs/E2B_MCP_COMPAT.md.
type catalogServer struct {
	command string
	args    []string
	// creds maps an E2B SDK credential key (as written in Sandbox.create({mcp}))
	// to the environment variable the underlying server reads. Keys not listed
	// here are passed through as UPPER_SNAKE_CASE (a best-effort fallback).
	creds map[string]string
}

// catalog is a curated, verified subset of the Docker MCP Catalog. Package names
// are confirmed present on npm; credential→env mappings are taken from the E2B
// docs' create({mcp}) examples and the Docker MCP Registry (server.yaml secrets).
// It is intentionally NOT the full 200+ catalog. Unlisted names are skipped with
// a warning telling the caller to use the explicit {installCmd,runCmd} form,
// because catalog image names don't follow a guessable convention (e.g. catalog
// "brave" → image mcp/brave-search; "github-official" → ghcr.io/github/…).
var catalog = map[string]catalogServer{
	// exa — docs: {exa:{apiKey}}; registry secret env EXA_API_KEY.
	"exa": {
		command: "npx", args: []string{"-y", "exa-mcp-server"},
		creds: map[string]string{"apiKey": "EXA_API_KEY"},
	},
	// brave search — registry secret env BRAVE_API_KEY.
	"brave": {
		command: "npx", args: []string{"-y", "@modelcontextprotocol/server-brave-search"},
		creds: map[string]string{"apiKey": "BRAVE_API_KEY"},
	},
	"brave-search": {
		command: "npx", args: []string{"-y", "@modelcontextprotocol/server-brave-search"},
		creds: map[string]string{"apiKey": "BRAVE_API_KEY"},
	},
	// github — registry secret env GITHUB_PERSONAL_ACCESS_TOKEN. Several credential
	// key spellings map to the same env so the exact SDK key need not be guessed.
	"github": {
		command: "npx", args: []string{"-y", "@modelcontextprotocol/server-github"},
		creds: map[string]string{
			"personalAccessToken":       "GITHUB_PERSONAL_ACCESS_TOKEN",
			"githubPersonalAccessToken": "GITHUB_PERSONAL_ACCESS_TOKEN",
			"token":                     "GITHUB_PERSONAL_ACCESS_TOKEN",
			"apiKey":                    "GITHUB_PERSONAL_ACCESS_TOKEN",
		},
	},
	"github-official": {
		command: "npx", args: []string{"-y", "@modelcontextprotocol/server-github"},
		creds: map[string]string{
			"personalAccessToken":       "GITHUB_PERSONAL_ACCESS_TOKEN",
			"githubPersonalAccessToken": "GITHUB_PERSONAL_ACCESS_TOKEN",
			"token":                     "GITHUB_PERSONAL_ACCESS_TOKEN",
			"apiKey":                    "GITHUB_PERSONAL_ACCESS_TOKEN",
		},
	},
	// airtable — docs: {airtable:{airtableApiKey}}.
	"airtable": {
		command: "npx", args: []string{"-y", "airtable-mcp-server"},
		creds: map[string]string{"airtableApiKey": "AIRTABLE_API_KEY", "apiKey": "AIRTABLE_API_KEY"},
	},
	// browserbase — docs: {browserbase:{apiKey,geminiApiKey,projectId}}.
	"browserbase": {
		command: "npx", args: []string{"-y", "@browserbasehq/mcp-server-browserbase"},
		creds: map[string]string{
			"apiKey":       "BROWSERBASE_API_KEY",
			"projectId":    "BROWSERBASE_PROJECT_ID",
			"geminiApiKey": "GEMINI_API_KEY",
		},
	},
	// slack — reference server; env SLACK_BOT_TOKEN + SLACK_TEAM_ID.
	"slack": {
		command: "npx", args: []string{"-y", "@modelcontextprotocol/server-slack"},
		creds: map[string]string{
			"botToken": "SLACK_BOT_TOKEN", "token": "SLACK_BOT_TOKEN",
			"teamId": "SLACK_TEAM_ID",
		},
	},
	// filesystem — no credentials; scoped to the sandbox workspace.
	"filesystem": {
		command: "npx", args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/workspace"},
	},
}

// catalogToServer resolves a Docker-MCP-Catalog entry ({name: {credentials}})
// to a stdio server config, injecting credentials as env vars. Returns ok=false
// when the name is not in the curated seed.
func catalogToServer(name string, creds map[string]any) (mcpbridge.ServerConfig, bool) {
	cs, ok := catalog[strings.ToLower(name)]
	if !ok {
		return mcpbridge.ServerConfig{}, false
	}
	sc := mcpbridge.ServerConfig{Command: cs.command, Args: cs.args}
	for k, v := range creds {
		s, isStr := v.(string)
		if !isStr || s == "" {
			continue
		}
		envName, mapped := cs.creds[k]
		if !mapped {
			envName = upperSnake(k) // best-effort for an unmapped credential key
		}
		if sc.Env == nil {
			sc.Env = map[string]string{}
		}
		sc.Env[envName] = s
	}
	return sc, true
}

// upperSnake converts a camelCase credential key to UPPER_SNAKE_CASE, e.g.
// "geminiApiKey" → "GEMINI_API_KEY". Used only as a fallback for credential keys
// not in a catalog entry's explicit mapping.
func upperSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' && i > 0 {
			b.WriteByte('_')
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
