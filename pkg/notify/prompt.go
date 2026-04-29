// Copyright 2026 SandrPod
// Cross-platform consent-prompt factory.
//
// Each platform's build-tag file (prompt_darwin.go / prompt_windows.go /
// prompt_linux.go) defines a `platformPrompter` type that implements
// permission.Notifier. NewPrompter() returns a value of that type.
//
// Why this layer exists:
//   - sandrpod-agent and sandrpod-tray both need a "the right notifier for
//     this OS" without sprinkling runtime.GOOS checks at the call site.
//   - Build tags + a factory keep every platform file self-contained: the
//     macOS file imports nothing Windows-specific, and vice-versa, so a
//     compilation issue in one file can't poison the others.
//
// Backward compatibility:
//   - The original `MacPrompter` type is preserved as an alias to keep
//     existing references compiling. New code should call NewPrompter().

package notify

import (
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// NewPrompter returns the OS-appropriate consent prompter.
// Returned value satisfies permission.Notifier.
func NewPrompter() permission.Notifier {
	return newPlatformPrompter()
}
