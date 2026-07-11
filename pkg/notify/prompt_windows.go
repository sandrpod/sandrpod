// Copyright 2026 SandrPod
// Windows consent-prompt implementation via PowerShell + WinForms MessageBox.
//
//go:build windows

package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sandrpod/sandrpod/pkg/brand"
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// WindowsPrompter implements permission.Notifier on Windows.
//
// Strategy:
//
//   We shell out to powershell.exe and run a tiny script that calls the
//   .NET WinForms MessageBox with YesNoCancel buttons.
//
// Why MessageBox and not toast / WPF?
//
//   - Toast (Windows.UI.Notifications): flashy and modern, but it's a
//     fire-and-forget surface — action buttons don't reliably return
//     a value to a non-UWP host process. Reverse-routing the click via
//     a notification protocol is significant work for a 3-button prompt.
//   - WPF custom dialog: full control over button labels, but adds ~150
//     lines of XAML embedded in PowerShell and pushes us into "the
//     developer is now an app-dev" territory.
//   - MessageBox: ships in every Windows since NT, modal, returns a
//     deterministic result code, runs even on locked-down corporate
//     installs where toast may be policy-disabled. Three-way return
//     (Yes/No/Cancel) maps cleanly onto our three responses.
//
// Trade-off:
//
//   MessageBox cannot relabel the Yes/No/Cancel buttons. We embed a
//   "[Yes = 永久允许] [No = 允许本次] [Cancel = 拒绝]" legend at the
//   top of the body so users see the mapping at a glance. The native
//   buttons stay in the OS language, which is acceptable because the
//   legend disambiguates and the dialog body itself is in Chinese.
//
// PowerShell startup overhead is ~200-400ms. Acceptable: prompts are
// not hot-path; the human takes seconds to read the body anyway.
type WindowsPrompter struct {
	AppTitle string
}

// NewWindowsPrompter constructs a WindowsPrompter with sensible defaults.
func NewWindowsPrompter() *WindowsPrompter {
	return &WindowsPrompter{AppTitle: brand.Name() + " Sandbox 权限请求"}
}

// newPlatformPrompter is the build-tag-selected constructor used by
// pkg/notify.NewPrompter().
func newPlatformPrompter() permission.Notifier { return NewWindowsPrompter() }

// Ask renders a 3-button MessageBox and maps the click to a PromptResponse:
//
//	Yes (6)    → PromptAllowPermanent
//	No (7)     → PromptAllowOnce
//	Cancel (2) → PromptDeny
//
// MessageBox is modal but does NOT honor any host-supplied timeout, so
// we drive the deadline via exec.CommandContext — when ctx fires, the
// PowerShell child process is killed and the dialog disappears. The
// user sees the dialog vanish; the manager records PromptTimeout.
func (p *WindowsPrompter) Ask(ctx context.Context, req permission.Request) (permission.PromptResponse, error) {
	// PTY consent: third option is session-scoped, not permanent (see
	// manager.CheckPTY). The legend text is the only thing differentiating
	// the two cases on Windows because MessageBox can't relabel buttons.
	yesMeaning := "永久允许"
	if req.Mode == permission.ModeExec {
		yesMeaning = "本会话允许"
	}
	legend := "[是 = " + yesMeaning + "]  [否 = 允许本次]  [取消 = 拒绝]`r`n`r`n"
	body := legend + buildPromptBody(req)

	// All single-quoted strings in PowerShell are literal — only doubled
	// single quotes get escaped. Replace ' → '' before interpolating.
	psBody := strings.ReplaceAll(body, "'", "''")
	psTitle := strings.ReplaceAll(p.AppTitle, "'", "''")

	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
$result = [System.Windows.Forms.MessageBox]::Show(
    '%s',
    '%s',
    [System.Windows.Forms.MessageBoxButtons]::YesNoCancel,
    [System.Windows.Forms.MessageBoxIcon]::Warning,
    [System.Windows.Forms.MessageBoxDefaultButton]::Button3
)
Write-Output $result.ToString()
`, psBody, psTitle)

	cmd := exec.CommandContext(ctx,
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", script,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Distinguish "we killed it because ctx fired" from real failure.
		if ctx.Err() != nil {
			return permission.PromptTimeout, nil
		}
		// Other errors (PS missing, .NET busted, …) — fail-close.
		return permission.PromptDeny, fmt.Errorf("powershell MessageBox failed: %v: %s", err, stderr.String())
	}

	answer := strings.TrimSpace(stdout.String())
	switch answer {
	case "Yes":
		// Yes maps to whichever permanence the legend told the user it would.
		if req.Mode == permission.ModeExec {
			return permission.PromptAllowSession, nil
		}
		return permission.PromptAllowPermanent, nil
	case "No":
		return permission.PromptAllowOnce, nil
	case "Cancel":
		return permission.PromptDeny, nil
	default:
		// Defensive — treat anything we don't recognize as deny so a
		// future PowerShell change can't accidentally fail-open.
		return permission.PromptDeny, fmt.Errorf("unexpected MessageBox response %q", answer)
	}
}

var (
	_ = errors.New // appease linters when no error path uses errors directly
)
