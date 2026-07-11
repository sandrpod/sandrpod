// Copyright 2026 SandrPod
// Tests for small helpers: default paths, tilde expansion, path containment,
// permanent-rule deny-skip, and the noop audit sink.

package permission

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultStorePath(t *testing.T) {
	p, err := DefaultStorePath()
	if err != nil {
		t.Fatalf("DefaultStorePath: %v", err)
	}
	if !strings.HasSuffix(p, filepath.Join(".sandrpod", "permissions.json")) {
		t.Errorf("unexpected store path: %q", p)
	}
}

func TestDefaultSocketPath(t *testing.T) {
	p, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	if !strings.HasSuffix(p, filepath.Join(".sandrpod", "authz.sock")) {
		t.Errorf("unexpected socket path: %q", p)
	}
}

func TestExpandTilde(t *testing.T) {
	home := "/Users/test"
	cases := []struct {
		in, want string
	}{
		{"~", home},
		{"~/Documents", filepath.Join(home, "Documents")},
		{"/absolute/path", "/absolute/path"}, // no tilde — unchanged
		{"~user/thing", "~user/thing"},       // ~user form unsupported — unchanged
		{"relative", "relative"},             // no leading tilde
	}
	for _, c := range cases {
		if got := expandTilde(c.in, home); got != c.want {
			t.Errorf("expandTilde(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Empty home short-circuits and returns input verbatim.
	if got := expandTilde("~/x", ""); got != "~/x" {
		t.Errorf("expandTilde with empty home = %q, want %q", got, "~/x")
	}
}

func TestPathInside(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true}, // identity
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/anything", "", false}, // empty parent never contains
	}
	for _, c := range cases {
		if got := pathInside(c.child, c.parent); got != c.want {
			t.Errorf("pathInside(%q,%q) = %v, want %v", c.child, c.parent, got, c.want)
		}
	}
}

// A permanent rule with Mode=="deny" must NOT grant access (matchAllow skips it).
func TestMatchAllow_DenyModeRuleDoesNotGrant(t *testing.T) {
	rules := []Rule{
		{Path: "/x", Mode: "deny", Scope: ScopePermanent},
	}
	req := Request{Path: "/x/file", Mode: ModeRead}
	if matchAllow(req, rules, "/home") {
		t.Error("a permanent rule with Mode=deny must not grant access")
	}
}

// A permanent rule whose granted mode is narrower than requested must not match.
func TestMatchAllow_ModeTooNarrow(t *testing.T) {
	rules := []Rule{
		{Path: "/x", Mode: ModeRead, Scope: ScopePermanent},
	}
	if matchAllow(Request{Path: "/x/f", Mode: ModeWrite}, rules, "/home") {
		t.Error("read-only rule must not satisfy a write request")
	}
}

// Session grants with no SessionID act as wildcards across sessions.
func TestMatchSessionAllow_WildcardSessionID(t *testing.T) {
	grants := []Rule{
		{Path: "/x", Mode: ModeRead, ExpiresAt: time.Now().Add(time.Hour)}, // no SessionID
	}
	req := Request{Path: "/x/f", Mode: ModeRead, SessionID: "anything"}
	if !matchSessionAllow(req, grants, "/home", time.Now()) {
		t.Error("a session grant with empty SessionID should match any session")
	}
}

// A grant scoped to a specific session must NOT leak to a sessionless request
// (SessionID == "") — the regression guard for the "this session only" consent
// leak. It still matches a request carrying its own session id.
func TestMatchSessionAllow_ScopedGrantDoesNotLeakToSessionless(t *testing.T) {
	grants := []Rule{
		{Path: "/x", Mode: ModeRead, SessionID: "sess-A", ExpiresAt: time.Now().Add(time.Hour)},
	}
	if matchSessionAllow(Request{Path: "/x/f", Mode: ModeRead, SessionID: ""}, grants, "/home", time.Now()) {
		t.Error("a session-scoped grant must not match a sessionless request")
	}
	if matchSessionAllow(Request{Path: "/x/f", Mode: ModeRead, SessionID: "sess-B"}, grants, "/home", time.Now()) {
		t.Error("a session-scoped grant must not match a different session")
	}
	if !matchSessionAllow(Request{Path: "/x/f", Mode: ModeRead, SessionID: "sess-A"}, grants, "/home", time.Now()) {
		t.Error("a session-scoped grant must match its own session")
	}
}

func TestNoopAuditSink_RecordDoesNothing(t *testing.T) {
	// Just exercise the noop sink; must not panic and is a no-op.
	noopAuditSink{}.Record("s", "d", "p", "m", "c", "sid", "r", "mc")
}

// recordDecision with an empty action must default to "unknown".
func TestRecordDecision_UnknownActionDefaulted(t *testing.T) {
	wd := t.TempDir()
	mgr, _ := newTestManager(t, wd, NopNotifier{})
	sink := &recordingSink{}
	mgr.SetAuditSink(sink)

	mgr.recordDecision("path", Decision{}, Request{Path: "/x"})
	if len(sink.records) != 1 {
		t.Fatalf("expected one record, got %d", len(sink.records))
	}
	if sink.records[0].decision != "unknown" {
		t.Errorf("empty action should record as 'unknown', got %q", sink.records[0].decision)
	}
}

// checkInternal is a thin pass-through to checkBody; cover it for completeness.
func TestCheckInternal_DelegatesToCheckBody(t *testing.T) {
	wd := t.TempDir()
	mgr, _ := newTestManager(t, wd, NopNotifier{})
	dec := mgr.checkInternal(context.Background(), Request{
		Path: filepath.Join(wd, "f.txt"),
		Mode: ModeRead,
	})
	if dec.Action != ActionAllow {
		t.Errorf("checkInternal should allow inside work_dir, got %+v", dec)
	}
}
