//go:build windows

// Copyright 2024 SandrPod
// platform_windows.go — Windows-specific shell and process helpers.
// Uses PowerShell as the system shell.

package toolbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// defaultWorkDir returns the current working directory on Windows
// (there is no /workspace container path convention).
func defaultWorkDir() string {
	wd, _ := os.Getwd()
	return wd
}

// nativeShell returns the PowerShell executable.
func nativeShell() string { return "powershell.exe" }

// nativeShellRunArgs returns flags for one-shot command execution via PowerShell.
func nativeShellRunArgs() []string {
	return []string{"-NoProfile", "-NonInteractive", "-Command"}
}

// nativeShellSessionArgs returns flags for launching a persistent PowerShell session.
func nativeShellSessionArgs() []string {
	// -NoExit keeps the session alive after each command.
	return []string{"-NoLogo", "-NoExit", "-NonInteractive"}
}

// setSysProcAttr is a no-op on Windows; Setpgid is not supported.
func setSysProcAttr(cmd *exec.Cmd) {}

// killProcess terminates the process on Windows.
func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill() //nolint:errcheck
	}
}

// toNativePath converts forward slashes to backslashes for Windows paths.
func toNativePath(p string) string {
	return strings.ReplaceAll(p, "/", "\\")
}

// buildCommandWrapper constructs a PowerShell snippet that runs command inside
// a persistent PowerShell session, captures all streams to logFile, and writes
// the numeric exit code to exitFile.
//
// Uses *> to redirect all output streams (stdout, stderr, warning, etc.).
// Falls back gracefully: $LASTEXITCODE is set by native executables;
// for PowerShell cmdlets we use $? (bool → 0/1).
func buildCommandWrapper(command, logFile, exitFile string) string {
	logFileW := toNativePath(logFile)
	exitFileW := toNativePath(exitFile)

	// Exit-code expression: prefer $LASTEXITCODE (native exes), fall back to $?
	exitExpr := `$(if($LASTEXITCODE -ne $null -and $LASTEXITCODE -ne 0){$LASTEXITCODE}elseif($?){0}else{1})`

	if strings.Contains(command, ">") {
		// User already has output redirection — just capture the exit code.
		return fmt.Sprintf(
			"%s; [System.IO.File]::WriteAllText('%s', \"%s\")\n",
			command, exitFileW, exitExpr,
		)
	}
	return fmt.Sprintf(
		"& { %s } *> '%s'; [System.IO.File]::WriteAllText('%s', \"%s\")\n",
		command, logFileW, exitFileW, exitExpr,
	)
}
