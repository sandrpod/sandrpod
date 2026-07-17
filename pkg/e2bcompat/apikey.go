// Copyright 2026 SandrPod Contributors
// E2B API-key handling. The E2B SDK validates keys client-side against
// /^e2b_[0-9a-f]+$/ (packages/js-sdk/src/api/index.ts), so keys handed to it
// must be in that shape. We therefore issue and accept e2b_<hex> keys and map
// them to SandrPod's own auth/ownership model.

package e2bcompat

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

// apiKeyPattern is exactly E2B's client-side validation.
var apiKeyPattern = regexp.MustCompile(`^e2b_[0-9a-f]+$`)

// IsE2BKey reports whether s has the shape the E2B SDK will accept.
func IsE2BKey(s string) bool { return apiKeyPattern.MatchString(s) }

// GenerateAPIKey returns a fresh e2b_<40 hex> key (E2B's canonical shape).
func GenerateAPIKey() (string, error) {
	b := make([]byte, 20) // 40 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "e2b_" + hex.EncodeToString(b), nil
}

// TokenFromKey strips the e2b_ prefix, yielding the opaque secret the rest of
// SandrPod can treat as a normal bearer token. A non-e2b key is returned as-is
// so operators may also front the gateway with an existing SandrPod token.
func TokenFromKey(key string) string {
	return strings.TrimPrefix(key, "e2b_")
}

// presentedKey extracts the credential from an E2B-style request: the SDK sends
// X-API-KEY for the control plane and, for envd, an access token via
// Authorization: Bearer.
func presentedKey(headerAPIKey, authorization string) string {
	if headerAPIKey != "" {
		return headerAPIKey
	}
	if after, ok := strings.CutPrefix(authorization, "Bearer "); ok {
		return after
	}
	return ""
}
