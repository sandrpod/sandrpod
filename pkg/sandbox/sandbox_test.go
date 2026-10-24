// Copyright 2026 SandrPod Contributors
// Tests for the sandbox plans. The mount plan and the profile are pure
// functions, so they are checked here on any platform; actually running them
// is what TestBwrap_RealExecution (Linux) and the seatbelt probe are for.

package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildBwrapArgs_Shape(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	ssh := filepath.Join(dir, "ssh")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(ssh, "id_rsa")
	if err := os.WriteFile(secret, []byte("KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	args, err := BuildBwrapArgs(Policy{WorkDir: work, ReadDeny: []string{ssh, secret}})
	if err != nil {
		t.Fatalf("BuildBwrapArgs: %v", err)
	}
	plan := strings.Join(args, " ")

	for _, want := range []string{
		"--ro-bind / /",                 // read-only root
		"--bind " + work + " " + work,   // the work tree
		"--tmpfs " + ssh,                // hard-locked directory masked
		"--ro-bind /dev/null " + secret, // hard-locked file masked
		"--unshare-pid",
		"--die-with-parent",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan is missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "--unshare-net") {
		t.Error("the network must stay unless NetworkNone is set")
	}

	// NetworkNone without a relay is a configuration error: the sandbox would
	// have no way to reach the proxy at all.
	if _, err := BuildBwrapArgs(Policy{WorkDir: work, NetworkNone: true}); err == nil {
		t.Fatal("NetworkNone without a Relay must be rejected")
	}

	relay := &Relay{
		Binary: "/usr/local/bin/sandrpod-agent", BinaryInSandbox: "/run/sandrpod/relay",
		SocketDir: dir, SocketDirInSandbox: "/run/sandrpod/egress",
		SocketName: "proxy.sock", Listen: "127.0.0.1:3128",
	}
	withNet, err := BuildBwrapArgs(Policy{WorkDir: work, NetworkNone: true, Relay: relay})
	if err != nil {
		t.Fatalf("BuildBwrapArgs with relay: %v", err)
	}
	netPlan := strings.Join(withNet, " ")
	for _, want := range []string{
		"--unshare-net",
		"--ro-bind /usr/local/bin/sandrpod-agent /run/sandrpod/relay",
		"--ro-bind " + dir + " /run/sandrpod/egress",
	} {
		if !strings.Contains(netPlan, want) {
			t.Errorf("netns plan is missing %q:\n%s", want, netPlan)
		}
	}
}

// A hardlock on a path that is not there must not make the plan invalid: bwrap
// refuses to mount a missing path, so those entries are dropped.
func TestBuildBwrapArgs_SkipsMissingDenyPaths(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope")
	args, err := BuildBwrapArgs(Policy{WorkDir: dir, ReadDeny: []string{missing, "~/not-a-thing-here"}})
	if err != nil {
		t.Fatalf("BuildBwrapArgs: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), missing) {
		t.Errorf("a missing deny path should be skipped:\n%v", args)
	}
}

// The root must never become writable, whatever the caller passes.
func TestBuildBwrapArgs_NeverBindsRootWritable(t *testing.T) {
	args, err := BuildBwrapArgs(Policy{WorkDir: "/", WriteAllow: []string{"/"}})
	if err != nil {
		t.Fatalf("BuildBwrapArgs: %v", err)
	}
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == "/" && args[i+2] == "/" {
			t.Fatalf("the plan binds / writable:\n%v", args)
		}
	}
}

func TestBuildSeatbeltProfile_Shape(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	ssh := filepath.Join(dir, "ssh")
	for _, p := range []string{work, ssh} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	profile, err := BuildSeatbeltProfile(Policy{WorkDir: work, ReadDeny: []string{ssh}})
	if err != nil {
		t.Fatalf("BuildSeatbeltProfile: %v", err)
	}
	if !strings.HasPrefix(profile, "(version 1)\n(deny default)\n") {
		t.Errorf("the profile must start from deny-by-default:\n%s", profile)
	}
	if !strings.Contains(profile, `(deny file-read* (subpath "`+ssh+`"))`) {
		t.Errorf("hardlock not denied:\n%s", profile)
	}
	if !strings.Contains(profile, `(allow file-write*`) ||
		!strings.Contains(profile, `(subpath "`+work+`")`) {
		t.Errorf("work_dir not writable:\n%s", profile)
	}
	if !strings.Contains(profile, "(allow network-outbound)") {
		t.Errorf("network should stay allowed by default:\n%s", profile)
	}
	// The deny must come after the global read allow (last match wins).
	if strings.Index(profile, "(allow file-read*)") > strings.Index(profile, "(deny file-read*") {
		t.Errorf("the read deny must follow the read allow:\n%s", profile)
	}

	noNet, err := BuildSeatbeltProfile(Policy{WorkDir: work, NetworkNone: true})
	if err != nil {
		t.Fatalf("BuildSeatbeltProfile: %v", err)
	}
	if strings.Contains(noNet, "(allow network-outbound)") {
		t.Error("NetworkNone should drop the network allow")
	}
}

func TestSbplString_Quoting(t *testing.T) {
	got := sbplString(`/tmp/a"b\c`)
	want := `"/tmp/a\"b\\c"`
	if got != want {
		t.Errorf("sbplString = %s, want %s", got, want)
	}
}

func TestResolve_Modes(t *testing.T) {
	if w, err := Resolve(ModeOff, "", ""); err != nil || w != nil {
		t.Errorf("off should resolve to no wrapper: %v, %v", w, err)
	}
	if _, err := Resolve("bogus", "", ""); err == nil {
		t.Error("an unknown mode must be an error")
	}
	// Forcing a backend this platform does not have is an error, not a silent
	// downgrade: the operator asked for a wall.
	if runtime.GOOS != "linux" {
		if _, err := Resolve(ModeBwrap, "/nonexistent/bwrap", ""); err == nil {
			t.Error("forcing bwrap where it cannot run must fail")
		}
	}
	if runtime.GOOS != "darwin" {
		if _, err := Resolve(ModeSeatbelt, "", "/nonexistent/sandbox-exec"); err == nil {
			t.Error("forcing seatbelt where it cannot run must fail")
		}
	}
}

func TestExpanded(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := expanded([]string{f, f, filepath.Join(dir, "missing"), "relative/path", ""})
	if len(got) != 1 || got[0] != f {
		t.Errorf("expanded = %v, want just %q", got, f)
	}
}

// The netns wrapper starts the relay before the command, and passes the real
// argv through "$@" rather than quoting it into the script.
func TestBwrapCommand_RelayWrap(t *testing.T) {
	relay := &Relay{
		Binary: "/bin/true", BinaryInSandbox: "/run/sandrpod/relay",
		SocketDir: t.TempDir(), SocketDirInSandbox: "/run/sandrpod/egress",
		SocketName: "proxy.sock", Listen: "127.0.0.1:3128",
	}
	w := &Bwrap{Path: "bwrap"}
	path, args, err := w.Command(Policy{WorkDir: t.TempDir(), NetworkNone: true, Relay: relay},
		"/usr/bin/python3", []string{"-c", `print("hi; rm -rf /")`})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if path != "bwrap" {
		t.Errorf("path = %q", path)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "/bin/sh\x00-c") {
		t.Fatalf("expected a shell wrapper, got %v", args)
	}
	if !strings.Contains(joined, `exec "$@"`) {
		t.Error("the wrapper must exec \"$@\" so argv is not re-parsed")
	}
	if !strings.Contains(joined, "relay --listen 127.0.0.1:3128 --unix \"/run/sandrpod/egress/proxy.sock\"") {
		t.Errorf("relay invocation missing:\n%s", joined)
	}
	// The dangerous argument must appear as an argv element, untouched.
	if args[len(args)-1] != `print("hi; rm -rf /")` {
		t.Errorf("the command's argument was rewritten: %q", args[len(args)-1])
	}
}
