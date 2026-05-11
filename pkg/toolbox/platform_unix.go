//go:build !windows

// Copyright 2024 SandrPod
// platform_unix.go — Unix/macOS specific shell and process helpers.

package toolbox

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

// platformOSVersion returns a human-readable OS distribution string.
// Reads PRETTY_NAME from /etc/os-release (Linux); returns runtime.GOOS
// ("darwin", "linux", ...) when the file is unavailable (macOS, BSD).
func platformOSVersion() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if val, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(val, `"`)
		}
	}
	return runtime.GOOS
}

// platformKernelVersion returns the kernel version string. On Linux this is
// the third field of /proc/version ("Linux version <ver> ..."); on macOS the
// file does not exist and we return an empty string.
func platformKernelVersion() string {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) >= 3 {
		return fields[2]
	}
	return ""
}

// defaultWorkDir returns /workspace when running inside a container, otherwise
// falls back to the current working directory (local-agent scenario on macOS/Linux).
func defaultWorkDir() string {
	const containerDir = "/workspace"
	if _, err := os.Stat(containerDir); err == nil {
		return containerDir
	}
	wd, _ := os.Getwd()
	return wd
}

// nativeShell returns the path to the system shell.
func nativeShell() string { return "/bin/bash" }

// nativePython returns the Python interpreter command on this platform.
func nativePython() string { return "python3" }

// prepareExecuteCode returns the code unchanged on Unix.
func prepareExecuteCode(code string) string { return code }

// nativeShellRunArgs returns flags for one-shot command execution.
func nativeShellRunArgs() []string { return []string{"-c"} }

// nativeShellSessionArgs returns flags for a persistent interactive session.
func nativeShellSessionArgs() []string { return []string{"-i"} }

// setSysProcAttr configures process-group isolation so that killing the parent
// also kills all child processes.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess terminates the process and its entire process group.
func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
	}
}

// buildCommandWrapper constructs a shell snippet that runs command inside a
// persistent bash session, captures all output to logFile, and writes the
// numeric exit code to exitFile.
func buildCommandWrapper(command, logFile, exitFile string) string {
	if strings.Contains(command, ">") {
		// User already has output redirection — just capture the exit code.
		return fmt.Sprintf("%s; echo $? > %s\n", command, exitFile)
	}
	return fmt.Sprintf("%s > %s 2>&1; echo $? > %s\n", command, logFile, exitFile)
}
