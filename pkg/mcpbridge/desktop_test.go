// Copyright 2026 SandrPod Contributors
package mcpbridge

import (
	"os"
	"testing"
)

// The desktop tools are advertised unconditionally but must fail with a clear
// message on a headless image — an agent asking "can I see the screen?" should
// get an answer, not a missing tool.
func TestDesktopAvailableHeadless(t *testing.T) {
	old := os.Getenv("DISPLAY")
	defer os.Setenv("DISPLAY", old)
	_ = os.Unsetenv("DISPLAY")
	if DesktopAvailable() {
		t.Fatal("DesktopAvailable() = true without DISPLAY")
	}
}

func TestDesktopToolsAdvertised(t *testing.T) {
	tools := DesktopTools()
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Tool.Name] = true
	}
	for _, want := range []string{"desktop_screenshot", "desktop_click", "desktop_type", "desktop_key", "desktop_scroll"} {
		if !names[want] {
			t.Fatalf("tool %q missing from installed desktop tools %v", want, names)
		}
	}
}
