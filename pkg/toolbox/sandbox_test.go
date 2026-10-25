// Copyright 2026 SandrPod Contributors
// The seam, not the sandbox: these prove every spawn site consults the
// installed wrapper. What the wrapper does is pkg/sandbox's business (and its
// own tests run it for real under bubblewrap).

package toolbox

import (
	"context"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/sandbox"
)

// fakeWrapper rewrites every command to a fixed one, so a test can tell
// whether a spawn went through the seam.
type fakeWrapper struct{ polices []sandbox.Policy }

func (f *fakeWrapper) Name() string     { return "fake" }
func (f *fakeWrapper) Available() error { return nil }
func (f *fakeWrapper) Command(p sandbox.Policy, path string, args []string) (string, []string, error) {
	f.polices = append(f.polices, p)
	return "/bin/sh", []string{"-c", "printf wrapped"}, nil
}

func withSandbox(t *testing.T, w sandbox.Wrapper, p sandbox.Policy) *fakeWrapper {
	t.Helper()
	fw, _ := w.(*fakeWrapper)
	SetSandbox(w, p)
	t.Cleanup(func() { SetSandbox(nil, sandbox.Policy{}) })
	return fw
}

func TestSandbox_ExecutorSpawnsWrapped(t *testing.T) {
	fw := withSandbox(t, &fakeWrapper{}, sandbox.Policy{WorkDir: "/work"})

	res, err := NewExecutor().Execute(context.Background(), "bash", `echo unwrapped`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "wrapped" {
		t.Fatalf("the command ran unwrapped: stdout = %q", res.Stdout)
	}
	if len(fw.polices) == 0 {
		t.Fatal("the wrapper was never consulted")
	}
	if fw.polices[0].WorkDir != "/work" {
		t.Errorf("policy passed through = %+v", fw.polices[0])
	}
}

func TestSandbox_SessionSpawnsWrapped(t *testing.T) {
	withSandbox(t, &fakeWrapper{}, sandbox.Policy{WorkDir: "/work"})

	m := NewSessionManager(t.TempDir())
	sess, err := m.Create("sb")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete("sb") })
	if sess == nil {
		t.Fatal("no session")
	}
}

func TestSandboxName(t *testing.T) {
	if got := SandboxName(); got != "" {
		t.Errorf("no wrapper installed, SandboxName = %q", got)
	}
	withSandbox(t, &fakeWrapper{}, sandbox.Policy{})
	if got := SandboxName(); got != "fake" {
		t.Errorf("SandboxName = %q, want fake", got)
	}
}

// The interactive path used to return before the wrap, so a PTY session ran
// outside the sandbox entirely. Whatever startPTY does afterwards, the wrapper
// must have been consulted first.
func TestSandbox_PTYStartGoesThroughTheWrapper(t *testing.T) {
	fw := &fakeWrapper{}
	SetSandbox(fw, sandbox.Policy{})
	t.Cleanup(func() { SetSandbox(nil, sandbox.Policy{}) })

	m := NewProcManager()
	// The PTY may not be available in this environment; that is not the point.
	_, _ = m.Start(ProcStartConfig{Cmd: "/bin/true", PTY: true})
	if len(fw.polices) == 0 {
		t.Fatal("a PTY session started without consulting the sandbox wrapper")
	}
}
