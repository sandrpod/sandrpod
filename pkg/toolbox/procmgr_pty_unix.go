//go:build !windows

// Copyright 2026 SandrPod Contributors
// PTY spawn/resize for the managed process table (Unix/macOS), via creack/pty.

package toolbox

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// startPTY starts cmd attached to a new pseudo-terminal and returns the master.
func startPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
}

// resizePTY changes the master's window size.
func resizePTY(ptmx *os.File, rows, cols uint16) error {
	return pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}
