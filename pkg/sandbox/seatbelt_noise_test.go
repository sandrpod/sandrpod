package sandbox

import (
	"strings"
	"testing"
)

func TestStripSeatbeltNoise(t *testing.T) {
	in := strings.Join([]string{
		"sandbox_extension_issue_file failed for /Library/Managed Preferences/cjzhao/com.apple.SystemConfiguration.plist: 1 (Operation not permitted)",
		"real output line",
		"sandbox_extension_issue_file failed for /Users/x/Library/Preferences/.GlobalPreferences.plist: 1 (Operation not permitted)",
		"deny file-write data /Users/x/sandrpod-work/.git/hooks/pre-commit",
		"",
	}, "\n")
	got := StripSeatbeltNoise(in)
	if strings.Contains(got, "sandbox_extension_issue_file") {
		t.Fatalf("noise survived:\n%s", got)
	}
	// A different refusal is real and must stay: it is the only evidence the
	// sandbox is doing its job.
	if !strings.Contains(got, "deny file-write data") {
		t.Fatalf("a real denial was dropped:\n%s", got)
	}
	if !strings.Contains(got, "real output line") {
		t.Fatalf("program output was dropped:\n%s", got)
	}
}

func TestStripSeatbeltNoise_NoNoiseUnchanged(t *testing.T) {
	in := "hello\nworld"
	if got := StripSeatbeltNoise(in); got != in {
		t.Fatalf("changed clean input: %q", got)
	}
}
