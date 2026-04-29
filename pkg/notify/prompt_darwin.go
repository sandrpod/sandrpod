// Copyright 2026 SandrPod
// macOS consent-prompt implementation via osascript.
//
//go:build darwin

package notify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// MacPrompter implements permission.Notifier by invoking osascript.
//
// Concurrent calls are serialized by the permission.Manager, but we still
// honor ctx so a slow user (or a stuck dialog) cannot block the agent forever.
type MacPrompter struct {
	// AppTitle is shown as the dialog window title.
	AppTitle string
}

// NewMacPrompter constructs a MacPrompter with sensible defaults.
func NewMacPrompter() *MacPrompter {
	return &MacPrompter{AppTitle: "Acme Sandbox 权限请求"}
}

// newPlatformPrompter is the build-tag-selected constructor used by
// pkg/notify.NewPrompter(). On macOS we return MacPrompter.
func newPlatformPrompter() permission.Notifier { return NewMacPrompter() }

// Ask renders a 3-button modal dialog and returns the employee's choice.
//
// Buttons: [拒绝] [允许本次] [永久允许]
//
// macOS `display dialog` enforces a hard maximum of 3 buttons (osascript
// error -50: "最多允许使用三个按钮"). We dropped the original
// "本会话允许" option — it was confusable with "允许本次" anyway, and
// session-scoped grants can still be installed via the tray settings page
// when needed (rare path).
//
// We use `display dialog` rather than `display notification` because:
//   - notification is fire-and-forget (no return value).
//   - dialog is modal, focus-stealing, and returns the clicked button.
//
// `giving up after` enforces a hard cap as a safety net; the manager also
// passes a ctx-derived deadline.
func (p *MacPrompter) Ask(ctx context.Context, req permission.Request) (permission.PromptResponse, error) {
	deadline := 25 // seconds — slightly under permission.PromptTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := int(timeUntil(dl)); remaining < deadline && remaining > 1 {
			deadline = remaining - 1
		}
	}

	body := buildPromptBody(req)
	script := fmt.Sprintf(`
		set theButtons to {"拒绝", "允许本次", "永久允许"}
		set theResult to display dialog %s ¬
			with title %s ¬
			buttons theButtons ¬
			default button "拒绝" ¬
			cancel button "拒绝" ¬
			with icon caution ¬
			giving up after %d
		set btn to button returned of theResult
		set giveUp to gave up of theResult
		if giveUp then
			return "TIMEOUT"
		end if
		return btn
	`,
		quoteAS(body),
		quoteAS(p.AppTitle),
		deadline,
	)

	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// `display dialog` exits with 1 when the user clicks the cancel
		// button — for our purposes that is a normal "deny", not an error.
		// osascript writes "User canceled. (-128)" to stderr in that case.
		if strings.Contains(stderr.String(), "-128") {
			return permission.PromptDeny, nil
		}
		// Any other failure (osascript missing, ctx cancelled, …) is an
		// infrastructure error — surface to the manager which will fail-close.
		if ctx.Err() != nil {
			return permission.PromptTimeout, nil
		}
		return permission.PromptDeny, fmt.Errorf("osascript failed: %v: %s", err, stderr.String())
	}

	answer := strings.TrimSpace(stdout.String())
	switch answer {
	case "允许本次":
		return permission.PromptAllowOnce, nil
	case "永久允许":
		return permission.PromptAllowPermanent, nil
	case "拒绝":
		return permission.PromptDeny, nil
	case "TIMEOUT":
		return permission.PromptTimeout, nil
	default:
		// Defensive: any unrecognized answer is treated as deny.
		return permission.PromptDeny, fmt.Errorf("unexpected osascript response %q", answer)
	}
}

// buildPromptBody / modeLabel moved to prompt_body.go (cross-platform).

// quoteAS escapes a Go string into an AppleScript string literal.
//
// AppleScript strings are wrapped in straight double-quotes; backslash is NOT
// special, but " must be doubled and we use \r/\n notation by reconstructing
// via concatenation. We keep this simple by replacing " with "" and converting
// newlines into AppleScript `& return &` concatenation, which is the standard
// idiom for multi-line dialog text.
func quoteAS(s string) string {
	// Split on \n and join with the AppleScript newline operator so the dialog
	// renders multiple lines correctly (osascript treats literal \n inside the
	// string as an opaque character, not a line break).
	parts := strings.Split(s, "\n")
	for i, part := range parts {
		parts[i] = `"` + strings.ReplaceAll(part, `"`, `\"`) + `"`
	}
	return strings.Join(parts, " & return & ")
}
