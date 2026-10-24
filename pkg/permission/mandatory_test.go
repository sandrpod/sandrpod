// Copyright 2026 SandrPod
// Mandatory deny: the paths that stay write-protected however generous the
// grants are. Two things are tested: the name matching (which must not catch
// innocent neighbours — .gitignore is not .git), and the walk that the OS
// sandbox uses to get concrete paths to mount over.

package permission

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIsMandatoryDenied(t *testing.T) {
	denied := []string{
		"/w/.git/hooks/pre-commit",
		"/w/sub/.git/hooks",
		"/w/.git/config",
		"/w/.bashrc",
		"/w/deep/nested/.zshrc",
		"/w/.mcp.json",
		"/w/.vscode/settings.json",
		"/w/.idea/workspace.xml",
		"/w/.claude/commands/deploy.md",
		"/w/.claude/agents/helper.md",
		"/w/.gitconfig",
		"/w/.ripgreprc",
	}
	for _, p := range denied {
		if !IsMandatoryDenied(p) {
			t.Errorf("%q should be mandatory-denied", p)
		}
	}

	allowed := []string{
		"/w/.gitignore",
		"/w/src/main.go",
		"/w/notes.md",
		"/w/.git", // a directory: the hooks/config inside it are the targets
		"/w/.github/workflows/ci.yml",
		"/w/.vscodeignore",
		"/w/README.md",
	}
	for _, p := range allowed {
		if IsMandatoryDenied(p) {
			t.Errorf("%q must stay writable", p)
		}
	}
}

// A `.git` *file* is a worktree/submodule pointer: rewriting it repoints the
// repository, so it is write-denied even though it is not a directory.
func TestIsMandatoryDenied_GitFile(t *testing.T) {
	dir := t.TempDir()
	gitFile := filepath.Join(dir, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsMandatoryDenied(gitFile) {
		t.Error("a .git file must be write-denied")
	}
}

func TestMandatoryDenyPaths_WalksTheTree(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, isDir bool) string {
		p := filepath.Join(root, rel)
		if isDir {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}

	topHook := mk(".git/hooks/pre-commit", false)
	topConfig := mk(".git/config", false)
	rc := mk(".bashrc", false)
	vs := mk(".vscode", true)
	nestedGit := mk("sub/repo/.git/hooks/post-commit", false)
	deep := mk("a/b/c/d/e/too-deep/.bashrc", false)
	plain := mk("src/main.go", false)

	got := MandatoryDenyPaths(root, 3)
	set := make(map[string]bool, len(got))
	for _, p := range got {
		set[p] = true
	}

	for _, want := range []string{
		filepath.Dir(topHook), // .git/hooks (the directory), not each hook file
		topConfig,
		rc,
		vs,
		filepath.Dir(nestedGit),
	} {
		if !set[want] {
			t.Errorf("missing %q from the deny list: %v", want, got)
		}
	}
	if set[plain] {
		t.Error("an ordinary source file must not be denied")
	}
	if set[deep] {
		t.Errorf("a path past the depth bound must not be listed: %q", deep)
	}
}

// depth 0 means "the root itself": no descent at all.
func TestMandatoryDenyPaths_DepthBound(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".bashrc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deep", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(root, "deep", "deeper", ".bashrc")
	if err := os.WriteFile(hidden, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := MandatoryDenyPaths(root, 0)
	for _, p := range got {
		if p == hidden {
			t.Fatalf("depth 0 listed a deep path: %v", got)
		}
	}
	found := false
	for _, p := range got {
		if p == filepath.Join(root, ".bashrc") {
			found = true
		}
	}
	if !found {
		t.Errorf("depth 0 should still list root-level files: %v", got)
	}
}

// The gate refuses a write even when the work tree was granted rw, and still
// allows reads of the same path.
func TestCheck_MandatoryDenyBeatsWorkDirAndGrant(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	notifier := &stubNotifier{resp: PromptAllowPermanent}
	mgr, store := newTestManager(t, root, notifier)
	if err := store.AddPermanentRule(Rule{Path: root, Mode: ModeReadWrite, Scope: ScopePermanent}); err != nil {
		t.Fatalf("AddPermanentRule: %v", err)
	}

	if dec := mgr.Check(context.Background(), Request{Path: hook, Mode: ModeWrite}); dec.Action != ActionDeny {
		t.Errorf("writing a git hook must be denied even with an rw grant on the tree: %+v", dec)
	}
	if dec := mgr.Check(context.Background(), Request{Path: hook, Mode: ModeRead}); dec.Action != ActionAllow {
		t.Errorf("reading it is fine: %+v", dec)
	}
	// An ordinary file in the same tree keeps working.
	plain := filepath.Join(root, "notes.md")
	if dec := mgr.Check(context.Background(), Request{Path: plain, Mode: ModeWrite}); dec.Action != ActionAllow {
		t.Errorf("an ordinary file must stay writable: %+v", dec)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("no prompts should be involved: %v", notifier.calls)
	}
}
