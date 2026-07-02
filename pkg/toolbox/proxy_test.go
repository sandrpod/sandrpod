package toolbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProxyPortHandler(t *testing.T) {
	// Backend "web app" on localhost with a random port.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s?%s", r.URL.Path, r.URL.RawQuery)
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	port := u.Port()

	s := &Server{}
	req := httptest.NewRequest("GET", "/proxy/"+port+"/api/x?q=1", nil)
	rec := httptest.NewRecorder()
	s.proxyPortHandler(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello from /api/x?q=1" {
		t.Errorf("body = %q", got)
	}
}

func TestProxyPortHandler_BadPort(t *testing.T) {
	s := &Server{}
	for _, p := range []string{"/proxy/abc/", "/proxy/0/", "/proxy/70000/x"} {
		rec := httptest.NewRecorder()
		s.proxyPortHandler(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", p, rec.Code)
		}
	}
}

func TestProxyPortHandler_NoService(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.proxyPortHandler(rec, httptest.NewRequest("GET", "/proxy/1/", nil)) // port 1: nothing listens
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no service reachable") {
		t.Errorf("body = %q", rec.Body.String())
	}
}
