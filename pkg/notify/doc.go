// Copyright 2026 SandrPod
//
// Package notify provides desktop-notification and consent-prompt
// implementations of the permission.Notifier interface.
//
// Sprint 1 ships only the macOS implementation (osascript-based modal
// dialog), gated by a build tag. Linux (notify-send + zenity) and Windows
// (toast + WPF dialog) follow in Sprint 4.
//
// Why osascript and not a Go-native library?
//   - Built-in on every macOS, no extra dependencies for sandrpod-agent.
//   - Native modal dialog with proper focus-stealing behavior so an
//     employee cannot easily miss the prompt.
//   - Returns user choice as plain text — trivial to parse with no FFI.
//
// Sprint 2 will move the prompt logic out of sandrpod-agent into a
// separate sandrpod-tray binary (so daemon and GUI have independent
// process lifetimes). The Notifier interface stays stable; only the
// transport changes (in-process call → unix-socket RPC).
package notify
