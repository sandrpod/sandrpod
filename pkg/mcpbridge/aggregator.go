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
