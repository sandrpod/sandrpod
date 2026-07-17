//go:build windows

// Copyright 2026 SandrPod Contributors
// platform_windows.go — Windows-specific shell and process helpers.
// Uses PowerShell as the system shell.

package toolbox

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// platformOSVersion returns a human-readable Windows version string. Shells
// out to `cmd /c ver` which prints e.g. "Microsoft Windows [Version 10.0.22631.4317]".
// Falls back to runtime.GOOS if the command fails (unlikely — cmd.exe is
// guaranteed on any Windows host that can run a Go binary).
func platformOSVersion() string {
	out, err := exec.Command("cmd", "/c", "ver").Output()
	if err == nil {
		s := strings.TrimSpace(string(out))
		if s != "" {
			return s
		}
	}
	return runtime.GOOS
}

// platformKernelVersion returns the NT kernel/build identifier. We extract
// the bracketed "[Version X.Y.Build.Rev]" tail from `cmd /c ver` output; if
// not present, return an empty string (mirrors the Unix behavior of "kernel
// version unknown on this host").
func platformKernelVersion() string {
	out, err := exec.Command("cmd", "/c", "ver").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	// "Microsoft Windows [Version 10.0.22631.4317]"
	if i := strings.LastIndex(s, "Version "); i >= 0 {
		ver := s[i+len("Version "):]
		ver = strings.TrimRight(ver, "]")
		return strings.TrimSpace(ver)
	}
	return ""
}

// defaultWorkDir returns the current working directory on Windows
// (there is no /workspace container path convention).
func defaultWorkDir() string {
	wd, _ := os.Getwd()
	return wd
}

// nativeShell returns the PowerShell executable.
func nativeShell() string { return "powershell.exe" }

// nativePython returns the Python interpreter command on Windows.
// Windows typically ships "python" (not "python3") in PATH.
func nativePython() string { return "python" }

// prepareExecuteCode prepends UTF-8 console encoding setup so that one-shot
// PowerShell executions emit UTF-8 bytes on stdout (captured via pipe by Go).
// chcp 65001 changes the OEM code page so native executables also output UTF-8.
func prepareExecuteCode(code string) string {
	const setup = `chcp 65001 | Out-Null; [Console]::OutputEncoding=$OutputEncoding=[System.Text.Encoding]::UTF8; `
	return setup + code
}

// nativeShellRunArgs returns flags for one-shot command execution via PowerShell.
func nativeShellRunArgs() []string {
	return []string{"-NoProfile", "-NonInteractive", "-Command"}
}

// nativeShellSessionArgs returns flags for launching a persistent PowerShell session.
// The -Command block runs once at startup and sets UTF-8 for all three encoding
// surfaces so that both stdin (commands we send) and stdout/file output are
// correctly handled as UTF-8, even on Chinese-locale Windows (CP936/GBK).
func nativeShellSessionArgs() []string {
	return []string{
		"-NoLogo", "-NoExit", "-NonInteractive",
		"-Command",
		`chcp 65001 | Out-Null; [Console]::InputEncoding=[Console]::OutputEncoding=$OutputEncoding=[System.Text.Encoding]::UTF8`,
	}
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
// Sets UTF-8 as the output encoding before running the command so the captured
// log file always contains valid UTF-8 (PowerShell 5.x defaults to UTF-16 LE).
// Uses *> to redirect all output streams (stdout, stderr, warning, etc.).
// Falls back gracefully: $LASTEXITCODE is set by native executables;
// for PowerShell cmdlets we use $? (bool → 0/1).
func buildCommandWrapper(command, logFile, exitFile string) string {
	logFileW := toNativePath(logFile)
	exitFileW := toNativePath(exitFile)

	// Force UTF-8 for stdin, stdout, and file output before every command.
	// chcp 65001 sets the Windows console code page to UTF-8 so that both
	// native-exe output and PowerShell cmdlet output are encoded correctly.
	encodingSetup := `chcp 65001 | Out-Null; [Console]::InputEncoding=[Console]::OutputEncoding=$OutputEncoding=[System.Text.Encoding]::UTF8; `

	// Exit-code expression: prefer $LASTEXITCODE (native exes), fall back to $?
	exitExpr := `$(if($LASTEXITCODE -ne $null -and $LASTEXITCODE -ne 0){$LASTEXITCODE}elseif($?){0}else{1})`

	if strings.Contains(command, ">") {
		// User already has output redirection — just capture the exit code.
		return fmt.Sprintf(
			"%s%s; [System.IO.File]::WriteAllText('%s', \"%s\")\n",
			encodingSetup, command, exitFileW, exitExpr,
		)
	}
	// Use Out-File -Encoding UTF8 instead of *> so the log file is always
	// UTF-8 encoded. PowerShell 5.x *> writes UTF-16 LE regardless of
	// $OutputEncoding; Out-File -Encoding UTF8 writes UTF-8 with BOM which
	// Go reads correctly.
	return fmt.Sprintf(
		"%s& { %s } 2>&1 | Out-File -Encoding UTF8 -FilePath '%s'; [System.IO.File]::WriteAllText('%s', \"%s\")\n",
		encodingSetup, command, logFileW, exitFileW, exitExpr,
	)
}
