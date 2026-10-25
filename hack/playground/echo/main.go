// Copyright 2026 SandrPod Contributors
// The playground's upstream: a tiny TLS server that reports what it received.
//
// It exists so a manual check can answer the only question that matters about
// credential masking — "did the real value actually arrive upstream?" — without
// needing an account at some third-party echo service. It prints the
// Authorization header it was given and returns it in the body.
//
//	go run ./hack/playground/echo -gen-ca ./certs        # writes ca.pem, cert.pem, key.pem
//	go run ./hack/playground/echo -addr :8443 -cert ./certs/cert.pem -key ./certs/key.pem
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	genDir := flag.String("gen-ca", "", "generate a CA and a server certificate into this directory, then exit")
	addr := flag.String("addr", ":8443", "listen address")
	certPath := flag.String("cert", "", "server certificate (PEM)")
	keyPath := flag.String("key", "", "server key (PEM)")
	flag.Parse()

	if *genDir != "" {
		if err := generate(*genDir); err != nil {
			log.Fatalf("gen-ca: %v", err)
		}
		return
	}
	if *certPath == "" || *keyPath == "" {
		log.Fatal("-cert and -key are required (or use -gen-ca)")
	}

	srv := &http.Server{
		Addr: *addr,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Advertise h1.1 only: the playground's whole point is to show the
			// proxy's terminated path, which speaks HTTP/1.1.
			NextProtos: []string{"http/1.1"},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			log.Printf("upstream: %s %s Authorization=%q", r.Method, r.URL.Path, auth)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"path":          r.URL.Path,
				"authorization": auth,
				"api_key":       r.Header.Get("X-Api-Key"),
				"host":          r.Host,
			})
		}),
	}
	log.Printf("upstream: listening on %s", *addr)
	log.Fatal(srv.ListenAndServeTLS(*certPath, *keyPath))
}

// generate writes a CA and a certificate for echo.local (plus localhost).
func generate(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Playground CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "echo.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"echo.local"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}

	write := func(name string, block *pem.Block) error {
		return os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644)
	}
	if err := write("ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER}); err != nil {
		return err
	}
	if err := write("cert.pem", &pem.Block{Type: "CERTIFICATE", Bytes: leafDER}); err != nil {
		return err
	}
	if err := write("key.pem", &pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER}); err != nil {
		return err
	}
	fmt.Printf("wrote %s/{ca.pem,cert.pem,key.pem}\n", dir)
	return nil
}
