// Copyright 2026 SandrPod
// Linux consent-prompt implementation via zenity (with kdialog fallback).
//
//go:build linux

package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// LinuxPrompter implements permission.Notifier on Linux desktops.
//
// Strategy:
//
//  1. Detect zenity (GNOME / GTK; ships on Ubuntu, Debian, Fedora, etc.).
//  2. Fall back to kdialog if zenity is missing (KDE).
//  3. If neither is available, fail-close with an error so the manager
//     denies the request and the operator sees an unmissable signal in
//     the audit log.
//
// Why not notify-send?
//
//   notify-send is fire-and-forget — it can't return the user's choice.
//   We need a modal that blocks until the human picks an option, which
//   is exactly what zenity --question is for.
//
// Why not GTK / Qt bindings?
//
//   Adding a real GUI library to the sandrpod-tray binary would explode
//   build complexity (CGO + native deps) for a feature that needs to
//   show three buttons. Spawning the desktop's own dialog binary is the
//   pragmatic choice — same approach the broader ecosystem uses
//   (BurntToast on Win, osascript on Mac, zenity on Linux).
type LinuxPrompter struct {
	AppTitle string
}

// NewLinuxPrompter constructs a LinuxPrompter with sensible defaults.
func NewLinuxPrompter() *LinuxPrompter {
	return &LinuxPrompter{AppTitle: "Acme Sandbox 权限请求"}
}

// newPlatformPrompter is the build-tag-selected constructor used by
// pkg/notify.NewPrompter().
func newPlatformPrompter() permission.Notifier { return NewLinuxPrompter() }

// Ask renders a 3-button modal dialog and returns the employee's choice.
//
// Buttons (zenity layout):
//   - cancel-label = "拒绝"     → exit code 1, stdout empty
//   - ok-label     = "允许本次"  → exit code 0
//   - extra-button = "永久允许"  → exit code 1, stdout = "永久允许"
//
// We disambiguate "拒绝" vs "永久允许" via stdout because zenity returns
// the same exit code 1 for both. This is a known quirk; see zenity(1).
func (p *LinuxPrompter) Ask(ctx context.Context, req permission.Request) (permission.PromptResponse, error) {
	if zenity, err := exec.LookPath("zenity"); err == nil {
		return p.askZenity(ctx, zenity, req)
	}
	if kdialog, err := exec.LookPath("kdialog"); err == nil {
		return p.askKDialog(ctx, kdialog, req)
	}
	return permission.PromptDeny, errors.New("no GUI prompter found: install `zenity` (GNOME) or `kdialog` (KDE)")
}

func (p *LinuxPrompter) askZenity(ctx context.Context, bin string, req permission.Request) (permission.PromptResponse, error) {
	body := buildPromptBody(req)

	// PTY consent uses session-scoped persistence; path consent uses permanent.
	// See pkg/permission/manager.go::CheckPTY for the rationale (shell sessions
	// shouldn't be set-and-forget; per-AI-conversation expiry is the right
	// granularity).
	thirdBtn := "永久允许"
	if req.Mode == permission.ModeExec {
		thirdBtn = "本会话允许"
	}

	args := []string{
		"--question",
		"--title=" + p.AppTitle,
		"--text=" + body,
		"--ok-label=允许本次",
		"--cancel-label=拒绝",
		"--extra-button=" + thirdBtn,
		"--width=520",
		"--icon-name=dialog-warning",
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	answer := strings.TrimSpace(stdout.String())

	// Exit code semantics:
	//   0  → ok-label clicked  → "允许本次"
	//   1  → cancel OR extra-button → distinguish via stdout
	//   5  → timed out (we don't pass --timeout but keep this branch defensive)
	//   any other → tooling failure
	if err == nil {
		return permission.PromptAllowOnce, nil
	}
	exitErr := &exec.ExitError{}
	if !errors.As(err, &exitErr) {
		// Process failed to start, was killed by our ctx, etc.
		if ctx.Err() != nil {
			return permission.PromptTimeout, nil
		}
		return permission.PromptDeny, fmt.Errorf("zenity failed: %v: %s", err, stderr.String())
	}
	switch exitErr.ExitCode() {
	case 1:
		if answer == "永久允许" {
			return permission.PromptAllowPermanent, nil
		}
		if answer == "本会话允许" {
			return permission.PromptAllowSession, nil
		}
		// Empty stdout → user clicked Cancel (拒绝) or closed the dialog.
		return permission.PromptDeny, nil
	case 5:
		return permission.PromptTimeout, nil
	default:
		// Anything else is treated as deny but surfaced for debugging.
		return permission.PromptDeny, fmt.Errorf("zenity unexpected exit %d: %s", exitErr.ExitCode(), stderr.String())
	}
}

// askKDialog uses kdialog --warningyesnocancel as the closest 3-way mapping.
//
// Mapping:
//   - Yes    → 永久允许
//   - No     → 允许本次
//   - Cancel → 拒绝
//
// kdialog cannot rename the buttons (--yes-label / --no-label exist in
// recent versions but are inconsistent across distros), so we add an
// explicit legend at the top of the prompt body explaining the mapping.
func (p *LinuxPrompter) askKDialog(ctx context.Context, bin string, req permission.Request) (permission.PromptResponse, error) {
	legend := "[Yes = 永久允许]  [No = 允许本次]  [Cancel = 拒绝]\n\n"
	body := legend + buildPromptBody(req)

	cmd := exec.CommandContext(ctx, bin,
		"--title", p.AppTitle,
		"--warningyesnocancel", body,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		// Yes button — kdialog returns 0
		return permission.PromptAllowPermanent, nil
	}
	exitErr := &exec.ExitError{}
	if !errors.As(err, &exitErr) {
		if ctx.Err() != nil {
			return permission.PromptTimeout, nil
		}
		return permission.PromptDeny, fmt.Errorf("kdialog failed: %v: %s", err, stderr.String())
	}
	switch exitErr.ExitCode() {
	case 1: // No → 允许本次
		return permission.PromptAllowOnce, nil
	case 2: // Cancel → 拒绝
		return permission.PromptDeny, nil
	default:
		return permission.PromptDeny, fmt.Errorf("kdialog unexpected exit %d: %s", exitErr.ExitCode(), stderr.String())
	}
}
