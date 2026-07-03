//go:build windows

// Copyright 2024 SandrPod
// PTY spawn/resize stubs for the managed process table on Windows. The toolbox
// runs in Linux sandbox containers in production; a Windows agent falls back to
// non-PTY processes, so E2B PTY commands are unsupported there.

package toolbox

import (
	"errors"
	"os"
	"os/exec"
)

var errPTYUnsupported = errors.New("PTY processes are not supported on Windows")

func startPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	return nil, errPTYUnsupported
}

func resizePTY(ptmx *os.File, rows, cols uint16) error {
	return errPTYUnsupported
}
