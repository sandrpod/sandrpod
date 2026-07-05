package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

// translateConfig converts the `--config` JSON into a sandrpod mcpbridge config.
// It accepts two shapes:
//
//   - sandrpod native: {"mcpServers": {"name": {"command": …}}}   → used as-is
//   - E2B `mcp` map:    {"name": {"installCmd","runCmd"}}          (custom server)
//     or {"name": {<credentials>}}                                 (Docker catalog)
//
// Custom-server entries (those carrying a runCmd) become a stdio child spawned
// via `sh -c`, cloning the repo first when the key is a GitHub path. Catalog-only
// entries (credentials, no run command) can't be resolved without the Docker MCP
// Catalog and are skipped with a warning.
func translateConfig(raw string) (*mcpbridge.Config, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &mcpbridge.Config{McpServers: map[string]mcpbridge.ServerConfig{}}, nil
	}

	// Native sandrpod shape (top-level mcpServers) → use directly.
	var native mcpbridge.Config
	if err := json.Unmarshal([]byte(raw), &native); err == nil && native.McpServers != nil {
		return &native, nil
	}

	// E2B flat map: name -> object.
	var flat map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &flat); err != nil {
		return nil, fmt.Errorf("config is neither sandrpod {mcpServers} nor an E2B mcp map: %w", err)
	}
	out := &mcpbridge.Config{McpServers: make(map[string]mcpbridge.ServerConfig, len(flat))}
	for name, entry := range flat {
		sc, ok := e2bEntryToServer(name, entry)
		if !ok {
			log.Printf("mcp-gateway: skipping %q — a Docker-catalog server needs the catalog to resolve; "+
				"use the custom-server form {\"installCmd\":\"…\",\"runCmd\":\"…\"}", name)
			continue
		}
		out.McpServers[sanitizeName(name)] = sc
	}
	return out, nil
}

// e2bEntryToServer maps one E2B mcp entry to a stdio child. An entry carrying a
// runCmd is a custom server (run it directly); otherwise the entry is a Docker-
// MCP-Catalog reference whose value is the credentials object, resolved against
// the curated catalog seed. Returns ok=false only for an unresolvable catalog
// name (not in the seed and no runCmd).
func e2bEntryToServer(name string, e map[string]any) (mcpbridge.ServerConfig, bool) {
	run, _ := e["runCmd"].(string)
	if strings.TrimSpace(run) == "" {
		return catalogToServer(name, e)
	}
	install, _ := e["installCmd"].(string)

	// GitHub-repo key ("owner/repo" or "github/owner/repo") → clone into /tmp and
	// run there, mirroring E2B's clone→install→run. Other keys run directly.
	var shell string
	if isRepoKey(name) {
		url, dir := repoURLAndDir(name)
		shell = fmt.Sprintf("[ -d %s ] || git clone --depth 1 %s %s; cd %s", dir, url, dir, dir)
		if install != "" {
			shell += " && " + install
		}
		shell += " && " + run
	} else if install != "" {
		shell = install + " && " + run
	} else {
		shell = run
	}

	sc := mcpbridge.ServerConfig{Command: "sh", Args: []string{"-c", shell}}
	if env := stringMap(e["env"]); len(env) > 0 {
		sc.Env = env
	}
	return sc, true
}

func isRepoKey(name string) bool {
	return strings.HasPrefix(name, "github/") ||
		(strings.Count(name, "/") == 1 && !strings.ContainsAny(name, " \t"))
}

func repoURLAndDir(name string) (url, dir string) {
	slug := strings.TrimPrefix(name, "github/")
	return "https://github.com/" + slug, "/tmp/mcp-" + sanitizeName(slug)
}

func sanitizeName(name string) string {
	return strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(name)
}

func stringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}
