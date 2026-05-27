// Package mcpbridge aggregates one or more stdio MCP servers into a single
// Streamable-HTTP MCP endpoint.
//
// The bridge spawns each configured server as an OS subprocess, speaks MCP
// over stdio with it (via mark3labs/mcp-go's client), and exposes the union
// of their tools / resources / prompts under a single HTTP endpoint with a
// namespace prefix (alias__tool).
//
// It is designed to be embedded inside sandrpod-agent but can also run
// standalone (see cmd/agent --mcp-only).
package mcpbridge
