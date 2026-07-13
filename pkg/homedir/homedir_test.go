package homedir

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsWindowsServiceProfile(t *testing.T) {
	svc := []string{
		// The exact path from the reported bug.
		`C:\Windows\System32\config\systemprofile`,
		`C:\Windows\System32\config\systemprofile\.sandrpod\skills`,
		`c:\windows\system32\config\systemprofile`, // case-insensitive
		`C:/Windows/System32/config/systemprofile`, // forward slashes
		`D:\Windows\SysWOW64\config\systemprofile`, // 32-bit variant, non-C drive
	}
	for _, p := range svc {
		if !IsWindowsServiceProfile(p) {
			t.Errorf("IsWindowsServiceProfile(%q) = false, want true", p)
		}
	}
	notSvc := []string{
		`C:\Users\alice`,
		`C:\Users\alice\.sandrpod`,
		`/home/alice`,
		`C:\Windows\System32`,        // System32 but not the service profile
		`C:\Users\systemprofile`,     // a user literally named systemprofile
		`C:\Windows\System32\config`, // config but not systemprofile
	}
	for _, p := range notSvc {
		if IsWindowsServiceProfile(p) {
			t.Errorf("IsWindowsServiceProfile(%q) = true, want false", p)
		}
	}
}

// On non-Windows, DataHome must be a plain home (never redirected) and DataDir
// must end in .sandrpod.
func TestDataDir(t *testing.T) {
	got := DataDir()
	if !strings.HasSuffix(filepath.ToSlash(got), "/.sandrpod") {
		t.Errorf("DataDir() = %q, want it to end in /.sandrpod", got)
	}
	if runtime.GOOS != "windows" && strings.Contains(strings.ToLower(got), "programdata") {
		t.Errorf("DataDir() unexpectedly redirected on %s: %q", runtime.GOOS, got)
	}
}
