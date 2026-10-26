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
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/egress"
	"github.com/sandrpod/sandrpod/pkg/relay"
)

// TestMain doubles as the relay: the wrapper starts the *test binary* with a
// "relay" subcommand, so no second artifact has to be built for the check.
func TestMain(m *testing.M) {
	// fetch is the sandboxed client for the masking test: Go's TLS stack reads
	// SSL_CERT_FILE, so a process inside the sandbox verifies the proxy's CA the
	// way any runtime does — no extra tool needed in the container.
	if len(os.Args) > 1 && os.Args[1] == "fetch" {
		fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
		rawURL := fs.String("url", "", "")
		hdr := fs.String("header", "", "Name: value")
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		req, err := http.NewRequest(http.MethodGet, *rawURL, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if name, val, ok := strings.Cut(*hdr, ":"); ok {
			req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(val))
		}
		// An explicit proxy, not ProxyFromEnvironment: Go deliberately never
		// proxies a loopback destination, and this test's upstream is on
		// loopback (in the field it is a real host, and the guard decides).
		client := http.DefaultClient
		proxyVar := os.Getenv("HTTPS_PROXY")
		if proxyVar == "" {
			proxyVar = os.Getenv("HTTP_PROXY")
		}
		if proxyVar != "" {
			pu, err := url.Parse(proxyVar)
			if err != nil {
				fmt.Fprintln(os.Stderr, "fetch: proxy:", err)
				os.Exit(1)
			}
			client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fetch:", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		fmt.Printf("%s\n%s\n", resp.Status, string(b))
		os.Exit(0)
	}
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
	// Decisions are recorded so the test can assert them.
	decisions := make(chan egress.Event, 16)
	srv, err := egress.Start(egress.Options{
		Policy:         compiled,
		UnixSocketPath: filepath.Join(sockDir, "proxy.sock"),
		Sink:           sinkFunc(func(e egress.Event) { decisions <- e }),
		LogAllowed:     true,
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

	// 2. Through the relay and the proxy, the allowed host is allowed. The
	//    assertion is the proxy's *decision*, not whether example.com answered:
	//    requiring a live fetch made this test fail on runners with a bad link,
	//    and "the runner had no route" was indistinguishable from "the proxy
	//    refused". The request still crosses loopback inside the namespace, so
	//    the relay and the socket are exercised either way.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, _ = run(proxyEnv, "wget -T 5 -q -O /dev/null http://example.com/ || true")
		if ev, ok := waitForDecision(decisions, "example.com", "allowed", 1*time.Second); ok {
			_ = ev
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("allowed host never reached the proxy")
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

// The full credential chain inside a real network sandbox: a process with no
// route of its own fetches HTTPS through the relay, the proxy terminates TLS,
// and the upstream receives the real credential while the sandbox only ever
// held a sentinel.
func TestNetnsRelay_MaskedCredentialReachesTheUpstream(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces here are a Linux feature")
	}
	w := &Bwrap{Path: "bwrap"}
	if err := w.Available(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}

	var seen string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(rw, "pong")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	upHost := upURL.Hostname()
	upPort, _ := strconv.Atoi(upURL.Port())

	base, err := os.MkdirTemp("/var/tmp", "sdn-mask-")
	if err != nil {
		t.Skipf("no scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	sockDir := filepath.Join(base, "sock")
	work := filepath.Join(base, "work")
	for _, d := range []string{sockDir, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	const real = "sk-real-credential-abcdef"
	mask, err := egress.NewMasker(map[string]string{"OPENAI_API_KEY": real})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := mask.Sentinels()["OPENAI_API_KEY"]

	ca, err := egress.LoadOrCreateCA(filepath.Join(base, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := egress.Parse(egress.Policy{
		AllowedDomains: []string{net.JoinHostPort(upHost, strconv.Itoa(upPort))},
	})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	srv, err := egress.Start(egress.Options{
		Policy:         compiled,
		CA:             ca,
		Mask:           mask,
		UnixSocketPath: filepath.Join(sockDir, "proxy.sock"),
		Terminate: &egress.TerminateOptions{
			CADir:         filepath.Join(base, "ca"),
			Ports:         []int{upPort},
			UpstreamRoots: roots,
		},
	})
	if err != nil {
		t.Fatalf("start egress: %v", err)
	}
	defer srv.Close()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
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
	// The CA path must be readable inside the sandbox; /var/tmp is not masked.
	if _, err := os.Stat(ca.CertPath()); err != nil {
		t.Fatalf("ca cert: %v", err)
	}

	// The command is the test binary itself, in its fetch mode: the sandbox
	// wrapper starts the relay and then execs this argv, so nothing is ever
	// pasted into a shell string.
	fetchArgs := []string{"fetch", "-url", upstream.URL,
		"-header", "Authorization: Bearer " + sentinel}
	env := []string{
		"HTTP_PROXY=http://sandbox:" + srv.Token() + "@127.0.0.1:3128",
		"HTTPS_PROXY=http://sandbox:" + srv.Token() + "@127.0.0.1:3128",
		"SSL_CERT_FILE=" + ca.CertPath(),
	}

	run := func() (string, error) {
		path, args, err := w.Command(policy, self, fetchArgs)
		if err != nil {
			t.Fatalf("Command: %v", err)
		}
		cmd := exec.Command(path, args...)
		cmd.Env = append(append(os.Environ(), env...), "HOME=/root")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		out, _ := run()
		if strings.Contains(out, "pong") {
			if strings.Contains(out, real) {
				t.Fatalf("the sandbox's own output contains the real credential:\n%s", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the masked request never completed:\n%s\nupstream saw %q", out, seen)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if seen != "Bearer "+real {
		t.Fatalf("upstream saw %q, want the real credential", seen)
	}
}

// sinkFunc adapts a function to egress.Sink, so a test can observe decisions.
type sinkFunc func(egress.Event)

func (f sinkFunc) RecordEgress(e egress.Event) { f(e) }

// waitForDecision waits for an event about host with the given code, returning
// the last event seen so a failure can say what actually happened.
func waitForDecision(ch <-chan egress.Event, host, code string, timeout time.Duration) (egress.Event, bool) {
	deadline := time.After(timeout)
	var last egress.Event
	for {
		select {
		case ev := <-ch:
			last = ev
			if ev.Host == host && ev.Code == code {
				return ev, true
			}
		case <-deadline:
			return last, false
		}
	}
}
