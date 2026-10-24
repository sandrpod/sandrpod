// Copyright 2026 SandrPod Contributors
// SigV4 re-signing, against AWS's own published example rather than against
// ourselves: a proxy that re-signs a request subtly wrong produces a signature
// mismatch that looks exactly like a wrong secret, and that is a bug which can
// hide for months.
package egress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The AWS documentation example: GET IAM, 2015-08-30, us-east-1.
// https://docs.aws.amazon.com/general/latest/gr/sigv4-signed-request-examples.html
func TestResignAWS_MatchesThePublishedExample(t *testing.T) {
	const (
		accessKey = "AKIDEXAMPLE"
		secret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		wantSig   = "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	)

	mask, err := NewMasker(map[string]string{
		"AWS_ACCESS_KEY_ID":     accessKey,
		"AWS_SECRET_ACCESS_KEY": secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mask.LinkAWS("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"); err != nil {
		t.Fatalf("LinkAWS: %v", err)
	}
	sentinel := mask.Sentinels()["AWS_ACCESS_KEY_ID"]

	req := httptest.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Host = "iam.amazonaws.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("X-Amz-Date", "20150830T123600Z")

	// What a sandboxed SDK would send: the sentinel in the credential scope.
	// The signature is over the sentinel request, which is what the proxy must
	// replace with a signature over the real credential.
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+sentinel+
		"/20150830/us-east-1/iam/aws4_request, "+
		"SignedHeaders=content-type;host;x-amz-date, Signature=deadbeef")

	resigned, err := mask.ResignAWS(req)
	if err != nil {
		t.Fatalf("ResignAWS: %v", err)
	}
	if !resigned {
		t.Fatal("ResignAWS did not recognise the request")
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential="+accessKey+"/20150830/us-east-1/iam/aws4_request") {
		t.Errorf("the credential scope still carries the sentinel: %s", auth)
	}
	if !strings.Contains(auth, "Signature="+wantSig) {
		t.Errorf("signature mismatch\n got: %s\nwant: %s", auth, wantSig)
	}
	if strings.Contains(auth, SentinelPrefix) {
		t.Fatalf("the sentinel survived re-signing: %s", auth)
	}
}

// A request that is not SigV4 is left alone.
func TestResignAWS_IgnoresOtherSchemes(t *testing.T) {
	mask, err := NewMasker(map[string]string{"GITHUB_TOKEN": "ghp_real"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+mask.Sentinels()["GITHUB_TOKEN"])
	resigned, err := mask.ResignAWS(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resigned {
		t.Fatal("a bearer token must not be re-signed as SigV4")
	}
	if !strings.Contains(req.Header.Get("Authorization"), SentinelPrefix) {
		t.Fatal("ResignAWS touched a non-SigV4 header")
	}
}

// An unlinked access key is not re-signed: the proxy cannot invent a secret.
func TestResignAWS_unlinkedKeyIsLeftAlone(t *testing.T) {
	mask, err := NewMasker(map[string]string{"AWS_ACCESS_KEY_ID": "AKID"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://s3.amazonaws.com/", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+mask.Sentinels()["AWS_ACCESS_KEY_ID"]+
		"/20250101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	if resigned, err := mask.ResignAWS(req); err != nil || resigned {
		t.Fatalf("resigned=%v err=%v; an unlinked key must be left as sent", resigned, err)
	}
}
