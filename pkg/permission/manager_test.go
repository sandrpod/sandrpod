// Copyright 2026 SandrPod
// End-to-end tests for the permission Manager.
//
// These cover the five decision branches in Check():
//   1. inside work_dir → silent allow
//   2. hardlock        → silent deny
//   3. permanent rule  → silent allow
//   4. session grant   → allow until expiry
//   5. unknown path    → notifier asked, response persisted

package permission

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// stubNotifier records the requests it sees and returns a canned response.
type stubNotifier struct {
	calls []Request
	resp  PromptResponse
	err   error
}

func (s *stubNotifier) Ask(ctx context.Context, req Request) (PromptResponse, error) {
	s.calls = append(s.calls, req)
	return s.resp, s.err
}

func newTestManager(t *testing.T, workDir string, notifier Notifier) (*Manager, *Store) {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "permissions.json")
	store, err := LoadStore(storePath)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	mgr, err := NewManager(Options{
		Store:    store,
		Notifier: notifier,
		WorkDir:  workDir,
		HomeDir:  "/Users/test",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, store
}

func TestCheck_InsideWorkDir_AllowedSilently(t *testing.T) {
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptDeny} // would refuse if asked
	mgr, _ := newTestManager(t, wd, notifier)

	dec := mgr.Check(context.Background(), Request{
		Path: filepath.Join(wd, "subdir/file.txt"),
		Mode: ModeRead,
	})
	if dec.Action != ActionAllow {
		t.Errorf("want ActionAllow, got %+v", dec)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("notifier should not be called for paths inside work_dir; got %d calls", len(notifier.calls))
	}
}

func TestCheck_Hardlock_DeniedSilently(t *testing.T) {
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptAllowPermanent} // would grant if asked
	mgr, store := newTestManager(t, wd, notifier)

	if err := store.AddHardlock("/Users/test/.ssh"); err != nil {
		t.Fatalf("AddHardlock: %v", err)
	}

	dec := mgr.Check(context.Background(), Request{
		Path: "/Users/test/.ssh/id_rsa",
		Mode: ModeRead,
	})
	if dec.Action != ActionDeny {
		t.Errorf("want ActionDeny, got %+v", dec)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("notifier should not be called for hardlocked paths; got %d calls", len(notifier.calls))
	}
}

func TestCheck_PermanentRule_AllowedSilently(t *testing.T) {
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptDeny}
	mgr, store := newTestManager(t, wd, notifier)

	_ = store.AddPermanentRule(Rule{Path: "/Users/test/Documents", Mode: ModeReadWrite})

	dec := mgr.Check(context.Background(), Request{
		Path: "/Users/test/Documents/2026/budget.xlsx",
		Mode: ModeRead,
	})
	if dec.Action != ActionAllow {
		t.Errorf("want ActionAllow, got %+v", dec)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("notifier should not be called when a permanent rule covers the path")
	}
}

func TestCheck_AskAndPermanent_PersistsRule(t *testing.T) {
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptAllowPermanent}
	mgr, store := newTestManager(t, wd, notifier)

	dec := mgr.Check(context.Background(), Request{
		Path: "/Users/test/code/project",
		Mode: ModeReadWrite,
	})
	if dec.Action != ActionAllow {
		t.Fatalf("want ActionAllow, got %+v", dec)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("want 1 notifier call, got %d", len(notifier.calls))
	}

	// Second request to the same subtree must not re-prompt.
	dec2 := mgr.Check(context.Background(), Request{
		Path: "/Users/test/code/project/sub/file",
		Mode: ModeRead,
	})
	if dec2.Action != ActionAllow {
		t.Errorf("second call should be allowed silently, got %+v", dec2)
	}
	if len(notifier.calls) != 1 {
		t.Errorf("notifier was re-called: now %d calls", len(notifier.calls))
	}

	// And the rule must be on disk.
	snap := store.Snapshot()
	if len(snap.Rules) != 1 || snap.Rules[0].Path != "/Users/test/code/project" {
		t.Errorf("permanent rule not persisted: %+v", snap.Rules)
	}
}

func TestCheck_AskAndDeny_NoPersist(t *testing.T) {
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptDeny}
	mgr, store := newTestManager(t, wd, notifier)

	dec := mgr.Check(context.Background(), Request{
		Path: "/Users/test/secret",
		Mode: ModeRead,
	})
	if dec.Action != ActionDeny {
		t.Errorf("want ActionDeny, got %+v", dec)
	}
	if len(store.Snapshot().Rules) != 0 {
		t.Errorf("denying must not persist any rule")
	}
}

func TestCheck_TimeoutFromNotifier_FailsClose(t *testing.T) {
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptTimeout}
	mgr, _ := newTestManager(t, wd, notifier)

	dec := mgr.Check(context.Background(), Request{
		Path: "/Users/test/anywhere",
		Mode: ModeRead,
	})
	if dec.Action != ActionDeny {
		t.Errorf("timeout must fail-close, got %+v", dec)
	}
}

func TestCheck_SessionGrant_HonoredUntilExpiry(t *testing.T) {
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptDeny}
	mgr, store := newTestManager(t, wd, notifier)

	_ = store.AddSessionRule(Rule{
		Path:      "/Users/test/Desktop",
		Mode:      ModeRead,
		SessionID: "sess-1",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	dec := mgr.Check(context.Background(), Request{
		Path:      "/Users/test/Desktop/note.txt",
		Mode:      ModeRead,
		SessionID: "sess-1",
	})
	if dec.Action != ActionAllow {
		t.Errorf("session grant should allow, got %+v", dec)
	}

	// Different session id must not be covered.
	dec2 := mgr.Check(context.Background(), Request{
		Path:      "/Users/test/Desktop/note.txt",
		Mode:      ModeRead,
		SessionID: "sess-2",
	})
	if dec2.Action != ActionDeny {
		t.Errorf("session grant should be scoped to session id, got %+v", dec2)
	}
}

func TestCheck_HardlockBeatsPermanent(t *testing.T) {
	// Defense-in-depth: even if a permanent allow somehow exists for a
	// path that is also hardlocked, the hardlock must win.
	wd := t.TempDir()
	notifier := &stubNotifier{resp: PromptDeny}
	mgr, store := newTestManager(t, wd, notifier)

	_ = store.AddHardlock("/Users/test/.aws")
	// Try to add a permanent allow that should be blocked by upsertRule.
	_ = store.AddPermanentRule(Rule{Path: "/Users/test/.aws", Mode: ModeReadWrite})

	dec := mgr.Check(context.Background(), Request{
		Path: "/Users/test/.aws/credentials",
		Mode: ModeRead,
	})
	if dec.Action != ActionDeny {
		t.Errorf("hardlock must beat permanent rule, got %+v", dec)
	}
}
