package egress

// What the upstream is allowed to see. A terminated tunnel re-originates the
// request, so the proxy's own bookkeeping headers must not travel with it.
import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMasking_UpstreamNeverSeesProxyHeaders(t *testing.T) {
	var got http.Header
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	_, port := hostPort(upURL.Host, 443)

	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	mask, err := NewMasker(map[string]string{"KEY": "real-value"})
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
	req, _ := http.NewRequest(http.MethodGet, "https://"+upURL.Host+"/", nil)
	req.Header.Set("Authorization", "Bearer "+mask.Sentinels()["KEY"])
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Proxy-Connection", "keep-alive")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Connection"} {
		if v := got.Get(h); v != "" {
			t.Errorf("the upstream received %s: %q", h, v)
		}
	}
	// The credential itself must still arrive.
	if got.Get("Authorization") != "Bearer real-value" {
		t.Errorf("authorization = %q", got.Get("Authorization"))
	}
	_ = strings.TrimSpace("")
}
