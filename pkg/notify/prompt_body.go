// Copyright 2026 SandrPod
// Cross-platform helpers for rendering the consent-prompt body.
//
// Kept in its own file (no build tag) so prompt_darwin / prompt_linux /
// prompt_windows can all share the formatting without duplicating it
// across three OS-gated files.

package notify

import (
	"fmt"
	"strings"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// buildPromptBody composes the human-readable prompt text. Kept short and
// information-dense; long bodies wrap awkwardly across platforms.
//
// Output is platform-agnostic plain text. macOS osascript joins lines with
// AppleScript `& return &`, Linux zenity uses `\n`, Windows MessageBox
// uses `\r\n`. Each platform converter handles its own newline rules; we
// emit `\n` here and let them translate.
func buildPromptBody(req permission.Request) string {
	mode := modeLabel(req.Mode)
	lines := []string{
		fmt.Sprintf("AI 助手请求%s访问以下路径：", mode),
		"",
		req.Path,
	}
	if req.Reason != "" {
		lines = append(lines, "", "原因：", req.Reason)
	}
	if req.Caller != "" {
		lines = append(lines, "", "调用方：", req.Caller)
	}
	if req.SessionID != "" {
		short := req.SessionID
		if len(short) > 24 {
			short = short[:8] + "…" + short[len(short)-8:]
		}
		lines = append(lines, "", "会话：", short)
	}
	return strings.Join(lines, "\n")
}

// modeLabel renders an access-mode for human eyes.
func modeLabel(m permission.Mode) string {
	switch m {
	case permission.ModeRead:
		return "读取"
	case permission.ModeWrite:
		return "写入"
	case permission.ModeReadWrite:
		return "读写"
	case permission.ModeExec:
		return "执行"
	default:
		return string(m)
	}
}
