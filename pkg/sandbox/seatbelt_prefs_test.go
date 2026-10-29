package sandbox

import (
	"strings"
	"testing"
)

// Denying preference reads does not stop anything; it only makes every process
// that touches CFPreferences print a sandbox_extension_issue_file line to stderr,
// which then appears in the output of an ordinary command.
func TestSeatbelt_PreferenceReadsAllowed(t *testing.T) {
	prof, err := BuildSeatbeltProfile(Policy{
		WorkDir:  "/Users/x/sandrpod-work",
		ReadDeny: []string{"/Users/x/Library/Preferences/com.apple.SystemConfiguration.plist"},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	deny := strings.Index(prof, "deny file-read*")
	allow := strings.Index(prof, `(allow file-read* (subpath "/Library/Preferences"))`)
	if allow < 0 {
		t.Fatalf("no read allowance for preferences:\n%s", prof)
	}
	if allow < deny {
		t.Fatal("the allowance must come after the deny, or the deny wins")
	}
}
