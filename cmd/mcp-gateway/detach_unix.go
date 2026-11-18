//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the server in its own session so the SDK's foreground command
// returning does not take it down with the process group.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
