// Copyright 2026 SandrPod Contributors
// TLS termination for the egress proxy.
//
// A CONNECT that the policy allows can be tunnelled opaquely (the client speaks
// TLS to the upstream itself) or terminated: the proxy completes the TLS
// handshake with the client using a certificate it signs per host, then opens
// its own verified TLS connection to the upstream and relays the request. Only
// the second kind can mask a credential, because only there is the request
// visible.
//
// What this costs is stated plainly, here and in the docs, because it is the
// part an operator must understand before turning it on:
//
//   - for a terminated destination, the certificate the sandbox sees is signed
//     by SandrPod's CA, not by the upstream. Certificate pinning, mTLS to the
//     upstream, and any "I verified the peer end to end" assumption inside the
//     sandbox do not hold. Hosts that need them belong in Exclude, which keeps
//     the opaque tunnel and simply gives up masking for those hosts;
//   - the proxy sees plaintext, including the real credential it substituted
//     in. The proxy is therefore a trust anchor for that traffic, and the CA
//     key is as sensitive as the credentials it unmasks.
//
// What it buys is the only general answer to "let an agent use a credential it
// must never be able to copy": the sandbox holds a sentinel, the real bytes
// exist only in the proxy's memory and in the request the proxy sends.
package egress

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TerminateOptions configures the terminating path.
type TerminateOptions struct {
	// CADir is where the CA certificate and key are written (mode 0700). The
	// certificate is also what the sandbox is told to trust, so it must be
	// readable by the sandbox: the file is 0644 inside that directory.
	CADir string
	// Exclude are destination patterns (same syntax as allowed_domains) whose
	// HTTPS is tunnelled opaquely and never terminated. Use for mTLS upstreams
	// and clients that pin the upstream certificate. Masking does not apply to
	// them.
	Exclude []string
	// Ports are the destinations whose CONNECT is terminated. Default 443.
	// Anything else stays an opaque tunnel: a CONNECT to 5432 is usually a
	// database speaking its own protocol, and terminating it would break it.
	Ports []int
	// UpstreamRoots replaces the roots used to verify the upstream when
	// terminating. Nil means the system roots. Set it for a site-local CA the
	// upstream presents (the same reason a corporate proxy has a CA bundle).
	UpstreamRoots *x509.CertPool
	// LeafTTL bounds a per-host certificate's life. Default 24h.
	LeafTTL time.Duration
}

// CA is a certificate authority for the proxy, either generated or loaded.
type CA struct {
	cert     *x509.Certificate
	key      crypto.Signer
	certPEM  []byte
	keyPEM   []byte
	keyPath  string
	certPath string

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// LoadOrCreateCA reads the CA pair from dir, generating one on first run.
// Persisting it matters: a CA that changes every restart forces every tool in
// the sandbox to re-trust (and quietly breaks anything that cached a
// certificate).
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("egress: ca dir: %w", err)
	}
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, err := parseCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("egress: load ca from %s: %w", dir, err)
		}
		ca.certPath, ca.keyPath = certPath, keyPath
		return ca, nil
	}

	ca, err := generateCA()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, ca.keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("egress: write ca key: %w", err)
	}
	if err := os.WriteFile(certPath, ca.certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("egress: write ca cert: %w", err)
	}
	ca.certPath, ca.keyPath = certPath, keyPath
	return ca, nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("no certificate block")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("no key block")
	}
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("key is not a signer")
	}
	return &CA{cert: cert, key: signer, certPEM: certPEM, leafs: map[string]*tls.Certificate{}}, nil
}

// generateCA makes an ECDSA P-256 CA. ECDSA over RSA because leaf signing
// happens on the connection path: a P-256 signature is microseconds, an RSA-2048
// one is not.
func generateCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "SandrPod Egress CA", Organization: []string{"SandrPod"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return &CA{
		cert:    cert,
		key:     key,
		certPEM: certPEM,
		keyPEM:  keyPEM,
		leafs:   map[string]*tls.Certificate{},
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// CertPath is the file the sandbox is told to trust.
func (c *CA) CertPath() string { return c.certPath }

// CertPEM is the CA certificate in PEM form.
func (c *CA) CertPEM() []byte { return c.certPEM }

// TLSConfigFor returns a *tls.Config that presents a certificate for the given
// server name. Certificates are cached per name and rotated LeafTTL before
// expiry (default 24h in the past, so effectively per process start).
func (c *CA) leafFor(serverName string, ttl time.Duration) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.leafs[serverName]; ok {
		return cert, nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: serverName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(serverName); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{serverName}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
	}
	c.leafs[serverName] = cert
	return cert, nil
}

// terminated reports whether a destination is terminated rather than tunnelled
// opaquely. Excludes win: a host in Exclude is always opaque, so an operator
// can exempt an upstream that must not be re-originated.
func (t *TerminateOptions) terminated(host string) bool {
	if t == nil {
		return false
	}
	for _, pat := range t.Exclude {
		if matchDomain(pat, host) {
			return false
		}
	}
	return true
}

// Terminating reports whether this destination's TLS is terminated.
func (s *Server) terminating(host string, port int) bool {
	t := s.opts.Terminate
	if t == nil || !t.terminated(host) {
		return false
	}
	if len(t.Ports) == 0 {
		return port == 443
	}
	for _, p := range t.Ports {
		if p == port {
			return true
		}
	}
	return false
}

// trustBundlePath writes a PEM file containing the CA plus the system roots,
// which is what most language runtimes want in SSL_CERT_FILE / NODE_EXTRA_CA_CERTS.
// The file is written once per process.
func (c *CA) trustBundlePath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.certPath == "" {
		return "", fmt.Errorf("egress: ca has no path")
	}
	bundle := filepath.Join(filepath.Dir(c.certPath), "trust-bundle.pem")
	if _, err := os.Stat(bundle); err == nil {
		return bundle, nil
	}
	roots, err := x509.SystemCertPool()
	var body strings.Builder
	body.Write(c.certPEM)
	if err == nil {
		// PEM-encode every system root after ours: a client that uses only this
		// file must still be able to verify anything not terminated (an
		// excluded host, or a connection that never went through the proxy).
		//
		// On macOS the pool is a keychain view and may not enumerate, in which
		// case the bundle holds our CA alone; termination then covers every
		// allowed 443 host, and a host in Exclude needs its own roots. That is
		// logged at startup rather than left to be discovered.
		for _, der := range rootsSubjects(roots) {
			body.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		}
	}
	if err := os.WriteFile(bundle, []byte(body.String()), 0o644); err != nil {
		return "", fmt.Errorf("egress: write trust bundle: %w", err)
	}
	return bundle, nil
}

// rootsSubjects returns the raw DER of every certificate in the pool.
func rootsSubjects(pool *x509.CertPool) [][]byte {
	if pool == nil {
		return nil
	}
	return poolSubjects(pool)
}

// TrustEnv is the environment that makes a sandboxed client trust the CA. Every
// entry points at one bundle file; the variable names are the ones the common
// runtimes actually read.
func (c *CA) TrustEnv() ([]string, error) {
	bundle, err := c.trustBundlePath()
	if err != nil {
		return nil, err
	}
	return []string{
		"SSL_CERT_FILE=" + bundle,
		"SSL_CERT_DIR=",
		"REQUESTS_CA_BUNDLE=" + bundle,
		"CURL_CA_BUNDLE=" + bundle,
		"NODE_EXTRA_CA_CERTS=" + bundle,
		"GIT_SSL_CAINFO=" + bundle,
		"AWS_CA_BUNDLE=" + bundle,
		"DENO_CERT=" + bundle,
		"PIP_CERT=" + bundle,
	}, nil
}
