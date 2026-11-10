// Copyright 2026 SandrPod Contributors
// Built-in desktop tools: the first-party MCP surface a sandbox offers beside
// the user's configured MCP servers. A desktop sandbox can be driven by
// screenshot + mouse + keyboard; a headless sandbox answers with a clear error
// instead of a confusing one, because the tools are always advertised (the
// agent should be able to ask "can I see the screen?").

package mcpbridge

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// DesktopAvailable reports whether this sandbox has an X display and the tools
// to drive it. The toolbox image has neither; the desktop image has both.
func DesktopAvailable() bool {
	if os.Getenv("DISPLAY") == "" {
		return false
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		return false
	}
	// An X display env alone is not enough: the socket must exist.
	return true
}

func runX(cmdName string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmdName, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w (%s)", cmdName, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// DesktopTools builds the built-in tool set. Screenshot returns an image
// content block (base64 PNG), so a vision-capable model can look at the screen.
func DesktopTools() []server.ServerTool {
	requireDisplay := func() error {
		if !DesktopAvailable() {
			return fmt.Errorf("this sandbox has no desktop (no DISPLAY/xdotool) — create it with a desktop image")
		}
		return nil
	}
	return []server.ServerTool{
		{
			Tool: mcp.NewTool("desktop_screenshot",
				mcp.WithDescription("Capture the sandbox desktop as a PNG image.")),
			Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := requireDisplay(); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				path := fmt.Sprintf("/tmp/sandrpod-shot-%d.png", time.Now().UnixNano())
				if _, err := runX("scrot", "-o", path); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				data, err := os.ReadFile(path)
				_ = os.Remove(path)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return mcp.NewToolResultImage("desktop screenshot", base64.StdEncoding.EncodeToString(data), "image/png"), nil
			},
		},
		{
			Tool: mcp.NewTool("desktop_click",
				mcp.WithDescription("Click the desktop at (x, y). button is left (default), right or middle."),
				mcp.WithNumber("x", mcp.Required(), mcp.Description("x coordinate")),
				mcp.WithNumber("y", mcp.Required(), mcp.Description("y coordinate")),
				mcp.WithString("button", mcp.Description("left|right|middle (default left)")),
				mcp.WithBoolean("double", mcp.Description("double-click when true")),
			),
			Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := requireDisplay(); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				x, _ := req.RequireFloat("x")
				y, _ := req.RequireFloat("y")
				// xdotool takes a button NUMBER, not a name: "click left" is a
				// BadValue error at runtime, which surfaced only when the tool was
				// actually exercised.
				btn := map[string]string{"left": "1", "middle": "2", "right": "3"}[strings.ToLower(req.GetString("button", "left"))]
				if btn == "" {
					btn = "1"
				}
				args := []string{"mousemove", fmt.Sprintf("%d", int(x)), fmt.Sprintf("%d", int(y))}
				if _, err := runX("xdotool", args...); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				clickArgs := []string{"click", btn}
				if req.GetBool("double", false) {
					clickArgs = []string{"click", "--repeat", "2", "--delay", "80", btn}
				}
				if _, err := runX("xdotool", clickArgs...); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return mcp.NewToolResultText(fmt.Sprintf("clicked button %s at %d,%d", btn, int(x), int(y))), nil
			},
		},
		{
			Tool: mcp.NewTool("desktop_type",
				mcp.WithDescription("Type text into the focused window."),
				mcp.WithString("text", mcp.Required(), mcp.Description("text to type")),
			),
			Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := requireDisplay(); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				text, err := req.RequireString("text")
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				if _, err := runX("xdotool", "type", "--delay", "12", text); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return mcp.NewToolResultText("typed"), nil
			},
		},
		{
			Tool: mcp.NewTool("desktop_key",
				mcp.WithDescription("Press a key combination, e.g. ctrl+l, alt+Tab, Return, Escape."),
				mcp.WithString("keys", mcp.Required(), mcp.Description("xdotool key spec")),
			),
			Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := requireDisplay(); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				keys, err := req.RequireString("keys")
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				if _, err := runX("xdotool", "key", keys); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return mcp.NewToolResultText("pressed " + keys), nil
			},
		},
		{
			Tool: mcp.NewTool("desktop_scroll",
				mcp.WithDescription("Scroll the desktop: up/down by clicks (wheel steps)."),
				mcp.WithString("direction", mcp.Description("up|down (default down)")),
				mcp.WithNumber("clicks", mcp.Description("wheel steps (default 3)")),
			),
			Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := requireDisplay(); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				dir := req.GetString("direction", "down")
				clicks := int(req.GetFloat("clicks", 3))
				btn := "5" // wheel down
				if strings.EqualFold(dir, "up") {
					btn = "4"
				}
				for i := 0; i < clicks; i++ {
					if _, err := runX("xdotool", "click", btn); err != nil {
						return mcp.NewToolResultError(err.Error()), nil
					}
				}
				return mcp.NewToolResultText(fmt.Sprintf("scrolled %s x%d", dir, clicks)), nil
			},
		},
	}
}
