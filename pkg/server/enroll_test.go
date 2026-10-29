package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeEnrolment accepts one token for one worker id, and refuses everything else.
type fakeEnrolment struct {
	wantToken string
	wantID    string
	issued    int
}

func (f *fakeEnrolment) Enrol(token, poderID string) (Credential, error) {
	if token != f.wantToken || poderID != f.wantID {
		return Credential{}, ErrEnrolmentRefused
	}
	f.issued++
	return Credential{Token: "wk_test", ExpiresAt: time.Now().Add(90 * 24 * time.Hour)}, nil
}

func enrolMux(f *fakeEnrolment) http.Handler {
	mux := http.NewServeMux()
	mountEnrolment(mux, func(next http.HandlerFunc) http.HandlerFunc { return next }, f)
	return mux
}

// The happy path: a worker presents its invitation once and gets a credential back.
func TestEnrol_BodyCarriesCredential(t *testing.T) {
	f := &fakeEnrolment{wantToken: "jn_abc", wantID: "poder-0123456789"}
	srv := httptest.NewServer(enrolMux(f))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/poders/enroll",
		strings.NewReader(`{"poder_id":"poder-0123456789"}`))
	req.Header.Set("Authorization", "Bearer jn_abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (a credential must not be cached)", got)
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		PoderID   string `json:"poder_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Token != "wk_test" || body.PoderID != "poder-0123456789" || body.ExpiresAt == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

// Every refusal looks the same from outside: an attacker learns nothing about
// whether a token existed, expired, or was already used.
func TestEnrol_RefusalIsOpaque(t *testing.T) {
	f := &fakeEnrolment{wantToken: "jn_right", wantID: "poder-0123456789"}
	// No credential at all is 403 (there is nothing to check); a wrong one is 401.
	cases := map[string]int{"jn_wrong": http.StatusUnauthorized, "": http.StatusForbidden}
	for tok, want := range cases {
		srv := httptest.NewServer(enrolMux(f))
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/poders/enroll",
			strings.NewReader(`{"poder_id":"poder-0123456789"}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		srv.Close()
		if resp.StatusCode != want {
			t.Fatalf("token %q: status = %d, want %d", tok, resp.StatusCode, want)
		}
		if strings.Contains(string(body[:n]), "expired") {
			t.Errorf("the refusal explains too much: %q", string(body[:n]))
		}
	}
}

// The id becomes part of the credential's identity, so it is validated rather than
// trusted.
func TestEnrol_RejectsBadPoderID(t *testing.T) {
	f := &fakeEnrolment{}
	srv := httptest.NewServer(enrolMux(f))
	defer srv.Close()
	for _, id := range []string{"", "short", "has space", "poder/../etc", strings.Repeat("a", 65)} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/poders/enroll",
			strings.NewReader(`{"poder_id":"`+id+`"}`))
		req.Header.Set("Authorization", "Bearer jn_abc")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("poder_id %q: status = %d, want 400", id, resp.StatusCode)
		}
	}
	if f.issued != 0 {
		t.Fatal("a refused id reached the enrolment implementation")
	}
}

// GET is not an enrolment.
func TestEnrol_MethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(enrolMux(&fakeEnrolment{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/poders/enroll")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
