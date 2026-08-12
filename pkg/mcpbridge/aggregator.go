package mcpbridge

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// NewAggregatorServer builds an *mcp-go* MCPServer that fronts the children
// managed by mgr. Tool registrations are refreshed every time the manager
// reports a change (children up/down or reload).
func NewAggregatorServer(mgr *ChildManager) *server.MCPServer {
	s := server.NewMCPServer(
		"sandrpod-mcp-bridge",
		"0.1.0",
		server.WithToolCapabilities(true),
		// Base protocol: a server offering resources MUST declare the
		// capability, so without this a conforming client never sends
		// resources/read no matter what the bridge routes. subscribe=false
		// because MCP Apps does not need it and forwarding a child's
		// notifications/resources/updated would mean a second, bidirectional
		// channel; listChanged=true because the set really does change as
		// children come and go, same as tools.
		server.WithResourceCapabilities(false, true),
		server.WithRecovery(),
	)

	sync := func() {
		// Replace the entire tool set so removed children's tools vanish.
		current := mgr.AggregatedTools()
		tools := make([]server.ServerTool, 0, len(current))
		for _, t := range current {
			tools = append(tools, server.ServerTool{
				Tool:    t,
				Handler: makeProxyHandler(mgr, t.Name),
			})
		}
		s.SetTools(tools...)

		// Likewise for resources — the majority of deployments will have
		// none, and SetResources with an empty set is how a child that had
		// them stops advertising them.
		res := mgr.AggregatedResources()
		srv := make([]server.ServerResource, 0, len(res))
		for _, r := range res {
			srv = append(srv, server.ServerResource{
				Resource: r,
				Handler:  makeResourceProxyHandler(mgr, r.URI),
			})
		}
		s.SetResources(srv...)
	}

	sync()
	mgr.OnChange(sync)
	return s
}

func makeProxyHandler(mgr *ChildManager, fqName string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := mgr.Dispatch(ctx, fqName, req.Params.Arguments)
		if err != nil {
			// Surface dispatch errors as JSON-RPC errors. mcp-go's server
			// converts non-nil errors here into proper error responses.
			return nil, fmt.Errorf("mcp dispatch: %w", err)
		}
		return res, nil
	}
}

func makeResourceProxyHandler(mgr *ChildManager, fqURI string) server.ResourceHandlerFunc {
	return func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		res, err := mgr.DispatchResource(ctx, fqURI)
		if err != nil {
			return nil, fmt.Errorf("mcp resource dispatch: %w", err)
		}
		if res == nil {
			return nil, fmt.Errorf("mcp resource dispatch: %s returned no contents", fqURI)
		}
		// Hand the upstream contents back untouched. _meta rides along on
		// each one, and for MCP Apps that _meta is the whole security
		// declaration — ui.csp, ui.permissions, ui.domain. Rewriting or
		// rebuilding these would strip the sandbox policy off the HTML it
		// applies to.
		return res.Contents, nil
	}
}
