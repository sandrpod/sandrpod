package egress

// The whole path, not the function: a SigV4 request carrying a sentinel must be
// re-signed by the proxy. This is where a substitution ordering mistake hides.
import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMasking_SigV4SurvivesSubstitution(t *testing.T) {
	const (
		accessKey = "AKIDEXAMPLE"
		secret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	)
	var seenAuth, seenBody string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, port := hostPort(upURL.Host, 443)

	mask, err := NewMasker(map[string]string{
		"AWS_ACCESS_KEY_ID":     accessKey,
		"AWS_SECRET_ACCESS_KEY": secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mask.LinkAWS("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"); err != nil {
		t.Fatal(err)
	}
	sentinel := mask.Sentinels()["AWS_ACCESS_KEY_ID"]

	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	compiled, _ := Parse(Policy{AllowedDomains: []string{upURL.Host}})
	srv, err := Start(Options{
		Policy: compiled, CA: ca, Mask: mask,
		Terminate: &TerminateOptions{CADir: dir, Ports: []int{port}, UpstreamRoots: upstreamPool(upstream)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := clientThroughProxy(t, srv, ca)
	req, _ := http.NewRequest(http.MethodPost, "https://"+upURL.Host+"/", strings.NewReader(`{"a":1}`))
	req.Header.Set("X-Amz-Date", "20150830T123600Z")
	req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	// What an SDK inside the sandbox would produce.
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+sentinel+
		"/20150830/us-east-1/execute-api/aws4_request, "+
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=deadbeef")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if strings.Contains(seenAuth, SentinelPrefix) {
		t.Fatalf("the sentinel reached the upstream: %s", seenAuth)
	}
	if strings.Contains(seenAuth, "Signature=deadbeef") {
		t.Fatalf("the request was not re-signed: %s", seenAuth)
	}
	if !strings.Contains(seenAuth, "Credential="+accessKey+"/") {
		t.Fatalf("the credential scope was not rewritten: %s", seenAuth)
	}
	_ = seenBody
}
