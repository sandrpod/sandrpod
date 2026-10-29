// Copyright 2026 SandrPod Contributors
// Worker enrolment: turning a short-lived invitation into a machine credential.
//
// A join token is meant to be read off a screen and pasted once. A worker needs a
// credential for its whole life — it authenticates at every reconnect, after a
// reboot, after a network blip — so the two cannot be the same thing:
//
//   - a token that lives long enough to be a worker's credential is a token that
//     is worth stealing, and it was issued to a human who has long since forgotten
//     it;
//   - a token short-lived enough to be safe cannot be the credential, because the
//     first reboot after it expires takes the node out of the fleet — silently,
//     until somebody notices the console is empty.
//
// So the worker presents its invitation once, gets its own credential back, and
// keeps that. Revoking one machine then means revoking one machine, instead of
// every worker that joined with the same workspace's token.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Enrolment is what an embedding build implements to mint worker credentials. The
// standard build has none: a self-hosted server hands out static tokens and does
// not need the exchange.
type Enrolment interface {
	// Enrol exchanges a join token for a credential bound to poderID. It must
	// reject a join token that is expired, revoked or already used, and it must
	// return the credential only once.
	Enrol(joinToken, poderID string) (Credential, error)
}

// Credential is what a worker keeps.
type Credential struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ErrEnrolmentRefused covers an invitation that cannot be exchanged: expired,
// revoked, already used, or naming a worker that is not the one presenting it.
var ErrEnrolmentRefused = errors.New("enrolment refused")

// validatePoderID keeps the identity a worker claims to something a database and a
// URL can hold: the credential it gets back is bound to this string, so it decides
// which machine the credential belongs to.
func validatePoderID(id string) error {
	if len(id) < 8 || len(id) > 64 {
		return errors.New("poder_id must be 8-64 characters")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return errors.New("poder_id may only contain letters, digits, - and _")
		}
	}
	return nil
}

// mountEnrolment serves POST /api/v1/poders/enroll.
//
// The request is authenticated by the *invitation*, not by a session: the worker
// has nothing else yet. What comes back is the credential it will use from now on,
// which is why this endpoint must be reachable only over TLS and why the response
// is never cached.
func mountEnrolment(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, enrolment Enrolment) {
	if enrolment == nil {
		return
	}
	mux.HandleFunc("/api/v1/poders/enroll", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")

		var req struct {
			PoderID string `json:"poder_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := validatePoderID(req.PoderID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ident := IdentityFrom(r)
		join := presentedToken(r)
		if join == "" {
			http.Error(w, "Forbidden: enrolment requires a join token", http.StatusForbidden)
			return
		}

		cred, err := enrolment.Enrol(join, req.PoderID)
		if err != nil {
			// One message for every refusal: an attacker learns nothing about
			// whether a token existed, expired or was used.
			log.Printf("enrol: poder %s refused (tenant=%q): %v", req.PoderID, ident.Tenant, err)
			http.Error(w, "enrolment refused", http.StatusUnauthorized)
			return
		}
		log.Printf("enrol: poder %s issued a worker credential (tenant=%q, expires %s)",
			req.PoderID, ident.Tenant, cred.ExpiresAt.UTC().Format(time.RFC3339))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"token":      cred.Token,
			"expires_at": cred.ExpiresAt.UTC().Format(time.RFC3339),
			"poder_id":   req.PoderID,
		})
	}))
}

// presentedToken is the bearer token on a request, the same one the authenticator
// resolved. Kept here so the enrolment handler does not re-implement header parsing
// differently from authentication.
func presentedToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Sandrpod-Token"))
}

// NewWorkerCredential mints a credential in the shape the load balancer and the
// console both expect: a prefix that says what it is, and enough entropy that it
// cannot be guessed.
func NewWorkerCredential(prefix string) (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}
