// Copyright 2026 SandrPod Contributors
//
// Control frames for the PTY WebSocket.
//
// The socket carries two kinds of message: bytes for the shell, and a control
// message for the window size. The size used to be reachable only through a
// Telnet NAWS escape sequence, which is what a terminal emulator library sends
// and nothing else does — a browser terminal writing the obvious JSON had that
// JSON typed into the shell instead.
//
// A control frame is a small JSON object; anything else is input.

package toolbox

import (
	"encoding/json"
)

// maxControlFrame bounds what we are willing to parse as a control message. A
// shell pasting a big JSON document must not be mistaken for one.
const maxControlFrame = 256

type controlFrame struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// parseResize reports the size carried by a JSON control frame, and whether the
// message was one at all. A malformed or unrelated message is not: it is input,
// and the caller writes it to the shell.
func parseResize(msg []byte) (cols, rows int, ok bool) {
	if len(msg) < 2 || len(msg) > maxControlFrame || msg[0] != '{' || msg[len(msg)-1] != '}' {
		return 0, 0, false
	}
	var c controlFrame
	if err := json.Unmarshal(msg, &c); err != nil {
		return 0, 0, false
	}
	// Both dimensions are required and sane: a zero would collapse the shell.
	if c.Type != "resize" || c.Cols <= 0 || c.Rows <= 0 || c.Cols > 1000 || c.Rows > 1000 {
		return 0, 0, false
	}
	return c.Cols, c.Rows, true
}
