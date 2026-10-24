// Copyright 2026 SandrPod Contributors
// Credential masking: the sandbox never holds the real bytes.
//
// A credential is replaced inside the sandbox with a sentinel of the form
// `fake_value_<uuid4>`. The sandboxed process sees only that; the egress proxy
// substitutes sentinel -> real on the way out, and only to destinations the
// policy already allowed, over a connection the proxy itself terminates and
// re-originates with a verified certificate.
//
// Two things follow from that design and are enforced rather than promised:
//
//   - the substitution only happens on a TLS-terminated path. Injecting a real
//     credential into cleartext would hand it to everyone on the path, so
//     plain-HTTP requests carrying a sentinel are refused, not "best effort";
//   - the sentinel map lives in process memory only. It is never written to
//     disk, never logged, and the audit record names the credential, never its
//     value.
//
// The sentinel is padded to the real value's byte length when the real value is
// longer than the base sentinel, so a body substitution keeps Content-Length
// unchanged and the request needs no re-framing. A shorter real value keeps the
// 47-byte base sentinel; entropy comes from the uuid4 either way.
package egress

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// SentinelPrefix marks a masked value. A credential whose real value happens
// to contain this prefix must not be masked (it would be ambiguous), which
// NewMasker refuses rather than guessing.
const SentinelPrefix = "fake_value_"

// Credential is one secret the proxy is allowed to substitute.
type Credential struct {
	// Name is the logical name (`OPENAI_API_KEY`, `AWS_SECRET_ACCESS_KEY`,
	// or a label for a value extracted from a file). It is what the audit
	// record says; it is never the value.
	Name string
	// Value is the real secret. It stays in memory.
	Value string
	// AWSAccessKey / AWSSecret / AWSSessionToken link the three values SigV4
	// needs as one credential: a request signed with the sentinel access key
	// is re-signed with the real secret, and the trigger is the sentinel
	// appearing in the signature's credential scope.
	AWSAccessKey string
}

// Masker is the sentinel registry. It is safe for concurrent use: the proxy
// serves many tunnels at once.
type Masker struct {
	mu sync.RWMutex
	// byName is the sandbox-facing view: name -> sentinel.
	byName map[string]string
	// bySentinel is the egress-facing view: sentinel -> real value.
	bySentinel map[string]Credential
	// aws maps a sentinel access key id to the real triple.
	aws map[string]awsPair
	// plaintext allowed: substitution on cleartext requests
	allowPlaintext bool
}

type awsPair struct {
	accessKey    string
	secret       string
	sessionToken string
}

// NewMasker builds the registry. maskedNames maps a credential name to its real
// value. Names are used verbatim as the sandbox-side variable name when the
// caller injects env, and in audit records.
func NewMasker(values map[string]string) (*Masker, error) {
	m := &Masker{
		byName:     make(map[string]string, len(values)),
		bySentinel: make(map[string]Credential, len(values)),
		aws:        make(map[string]awsPair),
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		real := values[name]
		if real == "" {
			continue
		}
		if strings.Contains(real, SentinelPrefix) {
			return nil, fmt.Errorf("credential %q contains %q, which cannot be masked unambiguously",
				name, SentinelPrefix)
		}
		sentinel := mintSentinel(real)
		m.byName[name] = sentinel
		m.bySentinel[sentinel] = Credential{Name: name, Value: real}
	}
	return m, nil
}

// LinkAWS groups three values as one SigV4 credential. The access key is masked
// like any other value; the link is what lets the proxy notice that a request
// signed with the sentinel key must be re-signed with the real secret.
func (m *Masker) LinkAWS(accessKey, secret, sessionToken string) error {
	sentinel, ok := m.byName[accessKey]
	if !ok {
		return fmt.Errorf("aws access key id %q is not masked", accessKey)
	}
	if _, ok := m.byName[secret]; !ok {
		return fmt.Errorf("aws secret %q is not masked", secret)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aws[sentinel] = awsPair{
		accessKey:    m.bySentinel[m.byName[accessKey]].Value,
		secret:       m.bySentinel[m.byName[secret]].Value,
		sessionToken: sessionTokenValue(m, sessionToken),
	}
	return nil
}

func sessionTokenValue(m *Masker, name string) string {
	if sentinel, ok := m.byName[name]; ok {
		return m.bySentinel[sentinel].Value
	}
	return ""
}

// SetAllowPlaintext permits substitution on cleartext requests. Off by default,
// because it hands the real value to the network path; enabled only by an
// operator who explicitly asked for it.
func (m *Masker) SetAllowPlaintext(v bool) { m.allowPlaintext = v }

// AllowsPlaintext reports the setting (used by the proxy's refusal message).
func (m *Masker) AllowsPlaintext() bool { return m.allowPlaintext }

// Env returns the sandbox-facing environment: each masked name set to its
// sentinel. The caller merges this over the inherited environment, so the real
// value never reaches a child process.
func (m *Masker) Env() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.byName))
	names := make([]string, 0, len(m.byName))
	for n := range m.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, n+"="+m.byName[n])
	}
	return out
}

// Names returns the masked credential names, for logging and audit.
func (m *Masker) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.byName))
	for n := range m.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Len is the number of masked credentials.
func (m *Masker) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byName)
}

// HasSentinel reports whether b contains any sentinel. Checked before any
// substitution so the common case (no credentials in this request) costs one
// scan of the bytes we already have.
func HasSentinel(b []byte) bool { return bytes.Contains(b, []byte(SentinelPrefix)) }

// maskerFor is a nil-safe accessor: the proxy calls these on every request.
func (s *Server) masker() *Masker { return s.opts.Mask }

// Substitute replaces every sentinel in b with its real value. It returns the
// rewritten bytes, whether anything changed, and the credentials that were
// used (for audit).
func (m *Masker) Substitute(b []byte) ([]byte, bool, []string) {
	if m == nil || !HasSentinel(b) {
		return b, false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	used := make(map[string]struct{}, 4)
	out := b
	for sentinel, cred := range m.bySentinel {
		if !bytes.Contains(out, []byte(sentinel)) {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(sentinel), []byte(cred.Value))
		used[cred.Name] = struct{}{}
	}
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	return out, len(names) > 0, names
}

// substituteHeaders rewrites every header value in place. Header names are not
// substituted: a sentinel as a header name would be a malformed request, and
// silently rewriting it would hide a bug.
func (m *Masker) substituteHeaders(h http.Header) (used []string) {
	if m == nil {
		return nil
	}
	seen := make(map[string]struct{}, 4)
	for k, vals := range h {
		for i, v := range vals {
			out, changed, names := m.Substitute([]byte(v))
			if !changed {
				continue
			}
			h[k][i] = string(out)
			for _, n := range names {
				seen[n] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	for n := range seen {
		used = append(used, n)
	}
	sort.Strings(used)
	return used
}

// ─── AWS SigV4 ──────────────────────────────────────────────────────────────
//
// A request signed with the sentinel access key id cannot be forwarded as-is:
// the signature is an HMAC over the request, derived from the real secret. The
// only reliable trigger for re-signing is the sentinel appearing in the
// credential scope of the Authorization header, which is what the SDKs put
// there.

const awsSigV4Prefix = "AWS4-HMAC-SHA256"

// awsScope is a parsed Authorization header.
type awsScope struct {
	SentinelKey string
	Date        string
	Region      string
	Service     string
	SignedList  string // "host;x-amz-date", as carried
	Signature   string
}

// parseAWSAuth parses `AWS4-HMAC-SHA256 Credential=AK/20260101/region/svc/aws4_request, SignedHeaders=..., Signature=...`.
func parseAWSAuth(header string) (awsScope, bool) {
	if !strings.HasPrefix(header, awsSigV4Prefix+" ") {
		return awsScope{}, false
	}
	rest := strings.TrimPrefix(header, awsSigV4Prefix+" ")
	var out awsScope
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "Credential":
			segs := strings.Split(val, "/")
			if len(segs) != 5 {
				return awsScope{}, false
			}
			out.SentinelKey, out.Date, out.Region, out.Service = segs[0], segs[1], segs[2], segs[3]
			if segs[4] != "aws4_request" {
				return awsScope{}, false
			}
		case "SignedHeaders":
			out.SignedList = val
		case "Signature":
			out.Signature = val
		}
	}
	if out.SentinelKey == "" || out.SignedList == "" || out.Signature == "" {
		return awsScope{}, false
	}
	return out, true
}

// ResignAWS re-signs a request whose Authorization header carries a sentinel
// access key. It returns false when the request is not SigV4 or the sentinel is
// not a linked AWS credential.
//
// The payload hash comes from the X-Amz-Content-Sha256 header the SDK set, not
// from the body: the body may have been substituted, and a signature over a
// rewritten body would be wrong in a way that looks like a clock skew to the
// caller. When the body changed, the caller must not re-sign — see
// canResignBody.
func (m *Masker) ResignAWS(req *http.Request) (bool, error) {
	if m == nil {
		return false, nil
	}
	auth := req.Header.Get("Authorization")
	scope, ok := parseAWSAuth(auth)
	if !ok {
		return false, nil
	}
	m.mu.RLock()
	pair, linked := m.aws[scope.SentinelKey]
	m.mu.RUnlock()
	if !linked {
		return false, nil
	}

	payloadHash := req.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		// No pre-hashed body: hash what will actually be sent.
		payloadHash = "UNSIGNED-PAYLOAD"
		if req.ContentLength == 0 {
			payloadHash = emptySHA256
		}
	}

	amzDate := req.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return false, fmt.Errorf("sigv4: signed request without X-Amz-Date")
	}

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := canonicalQueryString(req.URL)

	// SignedHeaders must be re-read from the request, in the order the client
	// declared: the signature covers that exact list.
	signed := strings.Split(scope.SignedList, ";")
	var canonicalHeaders strings.Builder
	for _, name := range signed {
		canonicalHeaders.WriteString(strings.ToLower(name))
		canonicalHeaders.WriteString(":")
		switch strings.ToLower(name) {
		case "host":
			canonicalHeaders.WriteString(req.Host)
		default:
			canonicalHeaders.WriteString(strings.TrimSpace(req.Header.Get(name)))
		}
		canonicalHeaders.WriteString("\n")
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders.String(),
		scope.SignedList,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{scope.Date, scope.Region, scope.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		awsSigV4Prefix,
		amzDate,
		credentialScope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+pair.secret), scope.Date)
	key = hmacSHA256(key, scope.Region)
	key = hmacSHA256(key, scope.Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		awsSigV4Prefix, pair.accessKey, credentialScope, scope.SignedList, signature))
	if pair.sessionToken != "" {
		// The session token is part of the signed headers when present; the
		// value sent must be the real one, or the upstream rejects the request
		// with a signature mismatch.
		req.Header.Set("X-Amz-Security-Token", pair.sessionToken)
	}
	return true, nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalQueryString renders the query per SigV4: sorted by encoded name,
// then encoded value, with no trailing newline. Values are already encoded by
// the parser, so the raw form is what the client signed.
func canonicalQueryString(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape is the URI-encoding SigV4 specifies (RFC 3986 unreserved only,
// space as %20).
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%")
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

// mintSentinel renders `fake_value_<uuid4>`, padded with random lowercase
// alphanumerics to the real value's byte length when that is longer. The pad
// carries no entropy contract — the uuid4 does — so a modulo bias here is
// irrelevant; what the pad buys is a length-preserving substitution, which
// keeps Content-Length valid and lets the proxy forward the header verbatim.
func mintSentinel(real string) string {
	base := SentinelPrefix + uuid.NewString()
	if len(real) <= len(base) {
		return base
	}
	chars := "abcdefghijklmnopqrstuvwxyz0123456789_-"
	pad := make([]byte, len(real)-len(base))
	for i := range pad {
		pad[i] = chars[(int(uuid.New().ID())>>uint(i%16))&0x3f%len(chars)]
	}
	return base + string(pad)
}

// Sentinels exposes the name→sentinel map. It is what the sandbox-side
// injection uses; the real values are deliberately not reachable through it.
func (m *Masker) Sentinels() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.byName))
	for k, v := range m.byName {
		out[k] = v
	}
	return out
}
