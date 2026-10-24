// Copyright 2026 SandrPod Contributors
// The whole network-isolation chain, executed for real: sandbox with its own
// network namespace, no route anywhere, and the relay as the only path to the
// proxy. Linux only; skipped where bubblewrap is unusable.
//
// Run it in a privileged container:
//
//	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./pkg/sandbox -o /tmp/sandbox.test
//	docker run --rm --privileged -v /tmp:/mnt alpine:3.20 \
//	  sh -c 'apk add -q bubblewrap && /mnt/sandbox.test -test.v -test.run NetnsRelay'
package sandbox

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/egress"
	"github.com/sandrpod/sandrpod/pkg/relay"
)

// TestMain doubles as the relay: the wrapper starts the *test binary* with a
// "relay" subcommand, so no second artifact has to be built for the check.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "relay" {
		fs := flag.NewFlagSet("relay", flag.ContinueOnError)
		listen := fs.String("listen", "127.0.0.1:3128", "")
		unixPath := fs.String("unix", "", "")
		ready := fs.String("ready", "", "")
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		if err := relay.Serve(context.Background(), *listen, *unixPath, *ready); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestNetnsRelay_ProxyIsTheOnlyWayOut(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces here are a Linux feature")
	}
	w := &Bwrap{Path: "bwrap"}
	if err := w.Available(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("wget is the client this test drives")
	}

	// A short directory (unix socket paths are limited to ~108 bytes) outside
	// the temp tree, so it is not inside the sandbox's writable mounts.
	base, err := os.MkdirTemp("/var/tmp", "sdn-")
	if err != nil {
		t.Skipf("no scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	sockDir := filepath.Join(base, "sock")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	compiled, err := egress.Parse(egress.Policy{AllowedDomains: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := egress.Start(egress.Options{
		Policy:         compiled,
		UnixSocketPath: filepath.Join(sockDir, "proxy.sock"),
	})
	if err != nil {
		t.Fatalf("start egress on a unix socket: %v", err)
	}
	defer srv.Close()
	if srv.UnixSocket() == "" {
		t.Fatal("the proxy did not take the unix socket")
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	policy := Policy{
		WorkDir:     work,
		NetworkNone: true,
		Relay: &Relay{
			Binary:             self,
			BinaryInSandbox:    "/run/sandrpod/relay",
			SocketDir:          sockDir,
			SocketDirInSandbox: "/run/sandrpod/egress",
			SocketName:         "proxy.sock",
			Listen:             "127.0.0.1:3128",
		},
	}

	run := func(env []string, script string) (string, error) {
		path, args, err := w.Command(policy, "/bin/sh", []string{"-c", script})
		if err != nil {
			t.Fatalf("Command: %v", err)
		}
		cmd := exec.Command(path, args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	proxyEnv := []string{
		"HTTP_PROXY=http://sandbox:" + srv.Token() + "@127.0.0.1:3128",
		"http_proxy=http://sandbox:" + srv.Token() + "@127.0.0.1:3128",
	}

	// 1. There is no route out on its own.
	out, err := run(nil, "wget -T 5 -q -O /dev/null http://example.com/ && echo REACHED || echo NO-ROUTE")
	if strings.Contains(out, "REACHED") {
		t.Fatalf("the sandbox reached the internet without the proxy:\n%s", out)
	}
	if !strings.Contains(out, "NO-ROUTE") {
		t.Fatalf("unexpected output for the no-route check: %q (%v)", out, err)
	}

	// 2. Through the relay and the proxy, the allowed host works. Wait for the
	//    relay to be listening; the wrapper starts it in the background.
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _ = run(proxyEnv, "wget -T 5 -q -O /dev/null http://example.com/ && echo OK || echo FAIL")
		if strings.Contains(out, "OK") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("allowed host never went through: %s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 3. A host outside the policy is refused through the same path.
	out, _ = run(proxyEnv, "wget -T 5 -q -O /dev/null http://example.org/ && echo REACHED || echo DENIED")
	if !strings.Contains(out, "DENIED") {
		t.Fatalf("an unlisted host should have been refused: %s", out)
	}

	// A sandbox with no route and a working relay is the whole point: it can
	// reach the proxy and nothing else, and the proxy decides.
}
