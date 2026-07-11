package toolbox

import "testing"

// TestSessionCreate_RejectsPathTraversalID guards the session_id validation
// fix: Create must reject ids that would escape the sessions base dir or inject
// into the log/exit-file shell paths, the same guard Execute already applies.
func TestSessionCreate_RejectsPathTraversalID(t *testing.T) {
	m := NewSessionManager(t.TempDir())
	for _, bad := range []string{"../../tmp/pwned", "a/b", "x\x00y", "has space"} {
		if _, err := m.Create(bad); err == nil {
			t.Errorf("Create(%q) should reject invalid session_id", bad)
		}
	}
	// A well-formed id (and the empty→generated case) still works.
	if _, err := m.Create("good_id-123"); err != nil {
		t.Errorf("Create(valid) failed: %v", err)
	}
	if _, err := m.Create(""); err != nil {
		t.Errorf("Create(\"\") should auto-generate, got %v", err)
	}
}
