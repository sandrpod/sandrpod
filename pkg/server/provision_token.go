// Copyright 2026 SandrPod Contributors
// Credentials for self-provisioned workers.
//
// A VM the scheduler creates is bootstrapped over SSH (or the cloud's run-command
// API), and the command that starts the Poder carries a credential. Handing that
// credential the platform token means every provisioned machine holds an
// administrator key for as long as it lives — and the SSH hop that delivers it
// runs against a host key nobody has seen before, so a network attacker on the
// first boot gets it too.
//
// A provision token is scoped to one VM, carries the tenant that asked for it,
// authenticates as a worker (the tunnel handshake and the job poll, nothing
// else), and is signed with the platform secret so any instance can verify it
// without shared state. It is not short-lived in the enrolment sense: a poder
// keeps using it for its whole life, so the TTL is the longest a machine may
// serve, not the boot window. What shrinks is the privilege, which is the part
// that mattered.

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// installProvisionTokens gives the scheduler a per-VM credential factory when
// the server has a secret to sign with. With auth disabled there is nothing to
// sign and nothing to replace: provisioned workers keep starting without a
// credential, exactly as before.
func installProvisionTokens(s *podpkg.Scheduler, cfg Config) {
	if cfg.Token == "" {
		return
	}
	s.SetTokenMinter(func(vmID, tenant string) string {
		return MintProvisionToken(cfg.Token, vmID, tenant, ProvisionTokenTTL, time.Now())
	})
}

// ProvisionTokenPrefix marks the credential so it is never mistaken for an
// operator's static token or a DB-issued API key.
const ProvisionTokenPrefix = "pv1_"

// provisionTokenContext separates this signature from any other use of the same
// secret, so a token minted here cannot be replayed where a different kind of
// signature is expected.
const provisionTokenContext = "sandrpod/provision-token/v1\x00"

// ProvisionTokenTTL is how long a provisioned worker's credential works. It has
// to outlive the VM: the pod is reclaimed when it is idle, but a busy one can
// serve for months, and a token that expires underneath a running worker turns
// into a machine that silently stops taking jobs.
var ProvisionTokenTTL = 180 * 24 * time.Hour

// MintProvisionToken returns the credential for one provisioned VM, or "" when
// there is no secret to sign with (auth disabled: the caller passes no token at
// all, exactly as before).
func MintProvisionToken(secret, vmID, tenant string, ttl time.Duration, now time.Time) string {
	if secret == "" || vmID == "" {
		return ""
	}
	payload := strings.Join([]string{vmID, tenant, strconv.FormatInt(now.Add(ttl).Unix(), 10)}, "|")
	return ProvisionTokenPrefix +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(signProvisionPayload(secret, payload))
}

func signProvisionPayload(secret, payload string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(provisionTokenContext))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// verifyProvisionToken decides whether presented is a provision token this
// secret minted and has not expired. The returned identity is a worker scoped to
// the tenant that requested the VM.
func verifyProvisionToken(secret, presented string, now time.Time) (Identity, bool) {
	if secret == "" {
		return Identity{}, false
	}
	body, ok := strings.CutPrefix(presented, ProvisionTokenPrefix)
	if !ok {
		return Identity{}, false
	}
	encodedPayload, encodedSig, ok := strings.Cut(body, ".")
	if !ok {
		return Identity{}, false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return Identity{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(encodedSig)
	if err != nil {
		return Identity{}, false
	}
	payload := string(payloadBytes)
	if !hmac.Equal(sig, signProvisionPayload(secret, payload)) {
		return Identity{}, false
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return Identity{}, false
	}
	vmID, tenant := parts[0], parts[1]
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || vmID == "" {
		return Identity{}, false
	}
	if now.Unix() > exp {
		return Identity{}, false
	}
	return Identity{Name: "provision:" + vmID, Role: RoleWorker, Tenant: tenant}, true
}
