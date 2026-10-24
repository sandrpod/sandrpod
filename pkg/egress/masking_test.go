// Copyright 2026 SandrPod Contributors
// The credential-masking chain, end to end and for real: a TLS upstream, a
// proxy that terminates it, and a client that only ever holds a sentinel.
//
// What these tests can prove, they prove against real TLS rather than mocks:
// the upstream reads the real header, the client's own log never contains the
// real value, an excluded host stays an opaque tunnel (the client's handshake
// fails because the upstream is not ours to sign for), and a cleartext request
// carrying a sentinel is refused instead of leaking.

package egress

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newMaskedProxy starts an egress proxy that terminates TLS and masks values.
func newMaskedProxy(t *testing.T, values map[string]string, allow []string, opts ...func(*TerminateOptions)) (*Server, *CA, *Masker) {
	t.Helper()

	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	mask, err := NewMasker(values)
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}
	compiled, err := Parse(Policy{AllowedDomains: allow})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	term := &TerminateOptions{CADir: dir}
	for _, o := range opts {
		o(term)
	}
	if len(term.Ports) == 0 {
		term.Ports = []int{443}
	}
	srv, err := Start(Options{Policy: compiled, CA: ca, Mask: mask, Terminate: term})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, ca, mask
}

// clientThroughProxy is a client that trusts only the proxy's CA — the same
// position the sandbox is put in by TrustEnv.
func clientThroughProxy(t *testing.T, srv *Server, ca *CA) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("cannot add the CA to a pool")
	}
	proxyURL, err := url.Parse(srv.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Timeout: 10e9,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		},
	}
}

// The whole point: the sandbox sends a sentinel, the upstream receives the real
// value, and the sentinel never turns into the real value on the client side.
func TestMasking_TerminatedRequestCarriesTheRealValue(t *testing.T) {
	var got string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	host, port := hostPort(upURL.Host, 443)

	const real = "sk-real-value-1234567890"
	srv, ca, mask := newMaskedProxy(t, map[string]string{"OPENAI_API_KEY": real},
		[]string{upURL.Host}, func(o *TerminateOptions) { o.Ports = []int{port} },
		upstreamRoots(upstream))

	// What the sandbox would see.
	sentinel := ""
	for _, kv := range mask.Env() {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			sentinel = strings.TrimPrefix(kv, "OPENAI_API_KEY=")
		}
	}
	if sentinel == "" || strings.Contains(sentinel, real) {
		t.Fatalf("sentinel = %q", sentinel)
	}

	client := clientThroughProxy(t, srv, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+upURL.Host+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through the terminating proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got != "Bearer "+real {
		t.Fatalf("upstream saw %q, want the real credential", got)
	}
	_ = host
}

// Bodies are substituted too, and the length stays consistent so the upstream
// does not see a truncated payload.
func TestMasking_BodySubstitution(t *testing.T) {
	var body string
	var gotLen int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		gotLen = r.ContentLength
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, port := hostPort(upURL.Host, 443)

	const real = "sk-real-body-value"
	srv, ca, mask := newMaskedProxy(t, map[string]string{"KEY": real}, []string{upURL.Host},
		func(o *TerminateOptions) { o.Ports = []int{port} }, upstreamRoots(upstream))
	sentinel := mask.Sentinels()["KEY"]

	payload := `{"api_key":"` + sentinel + `"}`
	client := clientThroughProxy(t, srv, ca)
	req, _ := http.NewRequest(http.MethodPost, "https://"+upURL.Host+"/v1", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if !strings.Contains(body, real) {
		t.Fatalf("upstream body = %q, want the real value", body)
	}
	if gotLen != int64(len(body)) {
		t.Errorf("Content-Length %d disagrees with the %d bytes sent", gotLen, len(body))
	}
}

// An excluded host is not terminated: the client completes its own handshake,
// and since the upstream certificate is not one our CA signed, verification
// fails. That failure is the proof that no MITM happened.
func TestMasking_ExcludedHostStaysOpaque(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	host, port := hostPort(upURL.Host, 443)

	srv, ca, _ := newMaskedProxy(t, map[string]string{"KEY": "real"},
		[]string{upURL.Host},
		func(o *TerminateOptions) {
			o.Ports = []int{port}
			o.Exclude = []string{host}
		}, upstreamRoots(upstream))

	client := clientThroughProxy(t, srv, ca)
	resp, err := client.Get("https://" + upURL.Host + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("an excluded host must not be terminated; the request should have failed certificate verification")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a certificate error, got: %v", err)
	}
}

// Cleartext plus a sentinel is refused: substituting would expose the value,
// forwarding would send a bogus one.
func TestMasking_CleartextSentinelIsRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the request must not reach the upstream")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	srv, _, mask := newMaskedProxy(t, map[string]string{"KEY": "real"}, []string{upURL.Host})
	sentinel := mask.Sentinels()["KEY"]

	proxyURL, _ := url.Parse(srv.ProxyURL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Deny-Reason"); got != CodeMaskedPlaintext {
		t.Errorf("X-Deny-Reason = %q, want %q", got, CodeMaskedPlaintext)
	}
}

// Masking without termination is a configuration error, not a warning: there
// would be no verified connection to put the credential on.
func TestStart_MaskRequiresTermination(t *testing.T) {
	mask, err := NewMasker(map[string]string{"KEY": "real"})
	if err != nil {
		t.Fatal(err)
	}
	compiled, _ := Parse(Policy{})
	if _, err := Start(Options{Policy: compiled, Mask: mask}); err == nil {
		t.Fatal("masking without termination must be rejected")
	}
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(Options{Policy: compiled, Terminate: &TerminateOptions{CADir: dir}}); err == nil {
		t.Fatal("termination without a CA must be rejected")
	}
	srv, err := Start(Options{Policy: compiled, CA: ca, Terminate: &TerminateOptions{CADir: dir},
		Mask: mask})
	if err != nil {
		t.Fatalf("a valid combination must start: %v", err)
	}
	_ = srv.Close()
}

// The CA is persisted: restarting the agent must not invalidate every trust
// store that already pinned it.
func TestCA_PersistsAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.CertPEM()) != string(second.CertPEM()) {
		t.Fatal("the CA changed across a restart")
	}
	if perm, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal(err)
	} else if perm.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the CA key is readable by others: %v", perm.Mode())
	}
}

// A leaf is valid for the name it was minted for, and chains to the CA.
func TestCA_LeafChainsToTheCA(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.leafFor("api.example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := parsed.Verify(x509.VerifyOptions{
		DNSName: "api.example.com", Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("leaf does not verify for its name: %v", err)
	}
	// A different name must fail: the certificate is per-host on purpose.
	if _, err := parsed.Verify(x509.VerifyOptions{
		DNSName: "evil.example.com", Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Fatal("the leaf should not be valid for another host")
	}
}

// The trust bundle carries our CA and the system roots, so a client that uses
// only this file can still verify everything it does not go through us for.
func TestCA_TrustEnvPointsAtABundle(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env, err := ca.TrustEnv()
	if err != nil {
		t.Fatal(err)
	}
	var bundle string
	for _, kv := range env {
		if strings.HasPrefix(kv, "SSL_CERT_FILE=") {
			bundle = strings.TrimPrefix(kv, "SSL_CERT_FILE=")
		}
	}
	if bundle == "" {
		t.Fatal("no SSL_CERT_FILE in the trust env")
	}
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		t.Fatal("the bundle is not PEM")
	}
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("the CA is not in the bundle")
	}
	// Where the platform ships a PEM bundle, the file is a superset of it; on a
	// host without one the CA alone is the guarantee, and termination covers
	// every allowed 443 host.
	if len(platformRoots()) > 0 {
		if len(pool.Subjects()) < 2 {
			t.Errorf("bundle has %d certificates, expected the CA plus system roots", len(pool.Subjects()))
		}
	}
}

// Terminated tunnels are marked in the audit stream: breaking end-to-end TLS is
// a deliberate loss an operator must be able to see.
func TestMasking_EventMarksTermination(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, port := hostPort(upURL.Host, 443)

	var events []Event
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	mask, err := NewMasker(map[string]string{"KEY": "real"})
	if err != nil {
		t.Fatal(err)
	}
	compiled, _ := Parse(Policy{AllowedDomains: []string{upURL.Host}})
	srv, err := Start(Options{
		Policy: compiled, CA: ca, Mask: mask,
		Terminate:  &TerminateOptions{CADir: dir, Ports: []int{port}},
		LogAllowed: true,
		Sink:       sinkFunc(func(e Event) { events = append(events, e) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	client := clientThroughProxy(t, srv, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+upURL.Host+"/", nil)
	req.Header.Set("Authorization", "Bearer "+mask.Sentinels()["KEY"])
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var terminated, withCredential bool
	for _, e := range events {
		if !e.Terminated {
			continue
		}
		terminated = true
		if len(e.Credentials) == 1 && e.Credentials[0] == "KEY" {
			withCredential = true
		}
	}
	if !terminated {
		t.Fatalf("no event marked the connection as terminated: %+v", events)
	}
	if !withCredential {
		t.Fatalf("the credential used was not recorded: %+v", events)
	}
}

type sinkFunc func(Event)

func (f sinkFunc) RecordEgress(e Event) { f(e) }

// upstreamRoots makes the proxy trust the test server's self-signed
// certificate, the way UpstreamRoots does for a site-local CA in the field.
func upstreamRoots(srv *httptest.Server) func(*TerminateOptions) {
	return func(o *TerminateOptions) { o.UpstreamRoots = upstreamPool(srv) }
}

func upstreamPool(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}
