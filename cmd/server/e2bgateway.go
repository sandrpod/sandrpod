// Copyright 2024 SandrPod
// Wires the E2B-compatible gateway (pkg/e2bcompat) into the SandrPod server:
// implements its SandboxBackend over the scheduler/store and its EnvdBackend by
// proxying filesystem/process ops to the sandbox's toolbox over the tunnel.
//
// Activated by SANDRPOD_E2B_DOMAIN. When set, requests whose Host matches the
// E2B hostnames (api.<domain> and <port>-<sandboxID>.<domain>) are served by
// the gateway; everything else falls through to the normal SandrPod mux.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sandrpod/sandrpod/pkg/e2bcompat"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// e2bDeps bundles what the gateway backends need from the server.
type e2bDeps struct {
	cfg         serverConfig
	scheduler   *podpkg.Scheduler
	sandboxes   podpkg.SandboxRepository
	poders      podpkg.PoderRepository
	jobs        podpkg.JobRepository
	tunnelStore *tunnel.TunnelStore
	directStore *tunnel.TunnelStore
	owners      podpkg.TunnelOwnerRepository
}

// forwardE2B backs e2bcompat's Forwarder hook. In multi-instance load mode a
// sandbox's tunnel terminates on one node; if this envd/code request landed on a
// different node, reverse-proxy it to the owner and report handled. It mirrors
// sandboxTunnel's ownership resolution so both surfaces route identically.
// Returns false (serve locally) for the single-instance case, an already-
// forwarded request, a local tunnel, or no known peer owner.
func (d e2bDeps) forwardE2B(w http.ResponseWriter, r *http.Request, sandbox string) bool {
	if d.cfg.NodeURL == "" || d.owners == nil || r == nil || r.Header.Get(forwardedHeader) != "" {
		return false
	}
	sb, ok := d.sandboxes.Get(sandbox)
	if !ok {
		return false // unknown sandbox: let the normal path emit its 404
	}
	key, local := sb.PoderID, d.tunnelStore
	if strings.HasPrefix(sb.ProxyURL, "direct://") {
		key, local = sandbox, d.directStore
	}
	if _, ok := local.Get(key); ok {
		return false // tunnel is on this instance — serve locally
	}
	owner, found := d.owners.NodeFor(key)
	if !found || owner == d.cfg.NodeURL {
		return false // no live peer owner — serve locally (yields the normal offline error)
	}
	forwardToNode(owner, w, r)
	return true
}

// portProxy backs e2bcompat's PortProxy hook. A request to a generic host-port
// (<port>-<sandboxID>.<domain>/<path>, where the port is neither envd nor the
// code interpreter) is proxied through the sandbox's tunnel to the toolbox's
// /proxy/<port>/ mount, which reverse-proxies to 127.0.0.1:<port> inside the
// sandbox. This is how E2B exposes in-sandbox HTTP services — most importantly
// the MCP gateway on :50005 (Sandbox.getMcpUrl()). The streaming client is used
// so MCP Streamable HTTP (a POST that upgrades into a long-lived SSE response)
// flushes in real time. Cross-node forwarding is handled by the gateway calling
// forwardE2B first, so by here the tunnel is local. Returns true when handled.
func (d e2bDeps) portProxy(w http.ResponseWriter, r *http.Request, sandbox string, port int) bool {
	subpath := "proxy/" + strconv.Itoa(port) + r.URL.Path
	t, target, err := d.toolboxTarget(sandbox, subpath)
	if err != nil {
		return false // unknown/offline sandbox — let the control plane emit the 404
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	proxyHTTPStreaming(t, r, target, w)
	return true
}

// e2bHostRouter routes E2B-hostname requests to the gateway and everything else
// to next (the normal mux).
func e2bHostRouter(domain string, gateway, next http.Handler) http.Handler {
	suffix := "." + domain
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		if host == "api"+suffix || strings.HasSuffix(host, suffix) {
			gateway.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newE2BGateway builds the E2B-compatible handler.
func newE2BGateway(domain string, d e2bDeps) http.Handler {
	return e2bcompat.Handler(e2bcompat.Config{
		Domain:          domain,
		Auth:            d.authenticator(),
		Sandboxes:       &e2bSandboxBackend{d},
		Envd:            &e2bEnvdBackend{d},
		Code:            &e2bCodeBackend{d},
		SandboxResolver: d.resolveSingleSandbox,
		Forwarder:       d.forwardE2B,
		PortProxy:       d.portProxy,
	})
}

// resolveSingleSandbox returns the identity's sole running sandbox. Used in
// HTTP debug mode, where the SDK is pointed at a fixed E2B_SANDBOX_URL and the
// sandbox ID can't be read from the Host. Returns "" when there is not exactly
// one candidate (ambiguous → let the envd call 404 clearly).
func (d e2bDeps) resolveSingleSandbox(ident string) string {
	var match string
	for _, sb := range d.sandboxes.List() {
		if ident != "" && sb.Owner != "" && sb.Owner != ident {
			continue
		}
		if !strings.HasPrefix(sb.Name, "e2b") {
			continue // only sandboxes created via this gateway
		}
		if match != "" {
			return "" // ambiguous
		}
		match = sb.Name
	}
	return match
}

// e2bCodeBackend implements the stateful run_code surface by proxying to the
// sandbox's toolbox /code-interpreter/execute endpoint.
type e2bCodeBackend struct{ d e2bDeps }

func (b *e2bCodeBackend) RunCode(name, contextID, code string) (e2bcompat.CodeExecution, error) {
	reqBody := map[string]string{"code": code, "context_id": contextID}
	var res struct {
		Stdout  string           `json:"stdout"`
		Stderr  string           `json:"stderr"`
		Text    string           `json:"text"`
		Error   string           `json:"error"`
		Images  []string         `json:"images"`
		Results []map[string]any `json:"results"`
	}
	if err := b.d.toolboxJSON(name, http.MethodPost, "code-interpreter/execute", nil, reqBody, &res); err != nil {
		return e2bcompat.CodeExecution{}, err
	}
	return e2bcompat.CodeExecution{
		Stdout: res.Stdout, Stderr: res.Stderr, Text: res.Text, Error: res.Error,
		Images: res.Images, Results: res.Results,
	}, nil
}

func (b *e2bCodeBackend) CreateContext(name, language, cwd string) (e2bcompat.CodeContext, error) {
	var ctx e2bcompat.CodeContext
	err := b.d.toolboxJSON(name, http.MethodPost, "code-interpreter/contexts",
		nil, map[string]string{"language": language, "cwd": cwd}, &ctx)
	return ctx, err
}

func (b *e2bCodeBackend) ListContexts(name string) ([]e2bcompat.CodeContext, error) {
	var out []e2bcompat.CodeContext
	err := b.d.toolboxJSON(name, http.MethodGet, "code-interpreter/contexts", nil, nil, &out)
	return out, err
}

func (b *e2bCodeBackend) RemoveContext(name, contextID string) error {
	return b.d.toolboxJSON(name, http.MethodDelete, "code-interpreter/contexts/"+contextID, nil, nil, nil)
}

func (b *e2bCodeBackend) RestartContext(name, contextID string) error {
	return b.d.toolboxJSON(name, http.MethodPost, "code-interpreter/contexts/"+contextID+"/restart", nil, nil, nil)
}

// authenticator maps a presented E2B key to a SandrPod identity. When auth is
// disabled it accepts any e2b_<hex>-shaped key anonymously.
func (d e2bDeps) authenticator() e2bcompat.Authenticator {
	return func(key string) (string, bool) {
		if d.cfg.authDisabled() {
			return "", true // auth disabled: accept anything (incl. empty)
		}
		if key == "" {
			return "", false
		}
		if id, ok := resolveToken(d.cfg, key); ok {
			return id.Name, true
		}
		// Per-sandbox envd access tokens: the gateway mints these (envdToken)
		// and stores them in a sandbox label. The SDK presents them for envd /
		// code calls as Authorization: Bearer, so accept a key matching one and
		// authenticate as the owning sandbox's identity. resolveToken only knows
		// server tokens; without this, every envd op 401s once server auth is on
		// (the local, auth-off harness never exercised this path).
		return d.envdTokenIdentity(key)
	}
}

// envdTokenIdentity reports the identity owning the sandbox whose envd access
// token equals key. Constant-time compare avoids leaking the token via timing.
func (d e2bDeps) envdTokenIdentity(key string) (string, bool) {
	for _, sb := range d.sandboxes.List() {
		tok := sb.Labels[e2bEnvdTokenLabel]
		if tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(key)) == 1 {
			return sb.Owner, true
		}
	}
	return "", false
}

// ─── toolbox call helper ──────────────────────────────────────────────────────

// toolboxDo performs an HTTP call to a sandbox's toolbox over the tunnel and
// returns the response. subpath is the toolbox path (e.g. "files/info").
// toolboxTarget resolves the tunnel and URL for a sandbox's toolbox subpath,
// choosing the direct-agent or poder route by the sandbox's proxy URL.
func (d e2bDeps) toolboxTarget(name, subpath string) (*tunnel.PoderTunnel, string, error) {
	sb, ok := d.sandboxes.Get(name)
	if !ok {
		return nil, "", e2bcompat.NotFoundError{Msg: "sandbox not found"}
	}
	if strings.HasPrefix(sb.ProxyURL, "direct://") {
		dt, ok := d.directStore.Get(name)
		if !ok {
			return nil, "", e2bcompat.NotFoundError{Msg: "agent offline"}
		}
		return dt, "http://agent/" + subpath, nil
	}
	pt, ok := d.tunnelStore.Get(sb.PoderID)
	if !ok {
		return nil, "", e2bcompat.NotFoundError{Msg: "poder offline"}
	}
	return pt, "http://poder/toolbox/" + name + "/" + subpath, nil
}

// poderPodAction invokes a pod lifecycle action on the sandbox's poder
// ("stop" = pause, "start" = resume). Only poder-backed sandboxes support it.
func (d e2bDeps) poderPodAction(sb *podpkg.SandboxInfo, action string) error {
	pt, ok := d.tunnelStore.Get(sb.PoderID)
	if !ok {
		return e2bcompat.NotFoundError{Msg: "poder offline"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://poder/sandboxes/"+sb.Name+"/"+action, nil)
	if err != nil {
		return err
	}
	resp, err := pt.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("poder %s: %s", action, strings.TrimSpace(string(b)))
	}
	return nil
}

func (d e2bDeps) toolboxDo(name, method, subpath string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	t, target, err := d.toolboxTarget(name, subpath)
	if err != nil {
		return nil, err
	}
	if q := query.Encode(); q != "" {
		target += "?" + q
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		cancel()
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// toolboxStream opens a long-lived streaming request to the toolbox (e.g. the
// process event stream). Unlike toolboxDo it uses no request timeout; the
// returned ReadCloser cancels the underlying context on Close, tearing down the
// tunnel stream promptly when the client disconnects.
func (d e2bDeps) toolboxStream(name, method, subpath string, query url.Values) (io.ReadCloser, error) {
	t, target, err := d.toolboxTarget(name, subpath)
	if err != nil {
		return nil, err
	}
	if q := query.Encode(); q != "" {
		target += "?" + q
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			return nil, e2bcompat.NotFoundError{Msg: "process not found"}
		}
		return nil, fmt.Errorf("toolbox %s: %s", subpath, strings.TrimSpace(string(b)))
	}
	return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// cancelBody cancels a request context when the response body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}

// toolboxJSON does a toolbox call and decodes a JSON response into out.
func (d e2bDeps) toolboxJSON(name, method, subpath string, query url.Values, reqBody, out any) error {
	var body io.Reader
	ct := ""
	if reqBody != nil {
		b, _ := json.Marshal(reqBody)
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := d.toolboxDo(name, method, subpath, query, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return e2bcompat.NotFoundError{Msg: "not found"}
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("toolbox %s: %s", subpath, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ─── SandboxBackend ───────────────────────────────────────────────────────────

type e2bSandboxBackend struct{ d e2bDeps }

// e2bSubstrate reports which SandrPod substrate E2B-created sandboxes land on.
// Defaults to the local docker poder; set SANDRPOD_E2B_PROVIDER (+ _REGION,
// _INSTANCE_TYPE) to route the unmodified E2B SDK's Sandbox.create() onto a
// cloud provider (e.g. gcp / asia-east1-a / e2-medium) so it provisions real
// cloud VMs. Region/instance type are only consulted for non-local providers.
func e2bSubstrate() (provider, region, instanceType string) {
	provider = os.Getenv("SANDRPOD_E2B_PROVIDER")
	if provider == "" {
		return "local", "local", ""
	}
	region = os.Getenv("SANDRPOD_E2B_REGION")
	if region == "" {
		region = "local"
	}
	return provider, region, os.Getenv("SANDRPOD_E2B_INSTANCE_TYPE")
}

func (b *e2bSandboxBackend) CreateSandbox(ident string, req e2bcompat.NewSandbox) (e2bcompat.SandboxDetail, error) {
	name := "e2b" + randToken(8)
	provider, region, instanceType := e2bSubstrate()
	create := &podpkg.CreateSandboxRequest{
		Name:         name,
		Region:       region,
		ProviderType: provider,
		InstanceType: instanceType,
		TTLSeconds:   int64(req.Timeout),
	}
	// An E2B templateID that looks like a container image is used directly.
	if strings.ContainsAny(req.TemplateID, "/:") {
		create.ImageID = req.TemplateID
	}
	sb, _, err := runSandboxCreate(b.d.scheduler, b.d.sandboxes, b.d.poders, b.d.jobs, b.d.tunnelStore, create, "", ident)
	if err != nil {
		return e2bcompat.SandboxDetail{}, err
	}
	// Persist E2B metadata + template as labels so get/list round-trip them.
	if len(req.Metadata) > 0 || req.TemplateID != "" {
		_ = b.d.sandboxes.Update(name, func(s *podpkg.SandboxInfo) {
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			for k, v := range req.Metadata {
				s.Labels[e2bMetaPrefix+k] = v
			}
			if req.TemplateID != "" {
				s.Labels[e2bTemplateLabel] = req.TemplateID
			}
		})
		sb, _ = b.d.sandboxes.Get(name)
	}
	return b.detailFromInfo(sb), nil
}

func (b *e2bSandboxBackend) GetSandbox(ident, sandboxID string) (e2bcompat.SandboxDetail, bool) {
	sb, ok := b.d.sandboxes.Get(sandboxID)
	if !ok || !b.owns(ident, sb) {
		return e2bcompat.SandboxDetail{}, false
	}
	return b.detailFromInfo(sb), true
}

func (b *e2bSandboxBackend) ListSandboxes(ident string, filter map[string]string) []e2bcompat.ListedSandbox {
	var out []e2bcompat.ListedSandbox
	for _, sb := range b.d.sandboxes.List() {
		if !b.owns(ident, sb) {
			continue
		}
		meta := e2bMetaFromLabels(sb.Labels)
		if !metaMatches(meta, filter) {
			continue
		}
		out = append(out, e2bcompat.ListedSandbox{
			TemplateID: e2bTemplateFromLabels(sb.Labels), SandboxID: sb.Name, StartedAt: sb.CreatedAt,
			CPUCount: 2, MemoryMB: 512, State: stateFor(sb),
			EnvdVersion: e2bcompat.DefaultEnvdVersion, Metadata: meta,
		})
	}
	return out
}

func (b *e2bSandboxBackend) KillSandbox(ident, sandboxID string) bool {
	sb, ok := b.d.sandboxes.Get(sandboxID)
	if !ok || !b.owns(ident, sb) {
		return false
	}
	teardownSandbox(context.Background(), sb, b.d.sandboxes, b.d.poders, b.d.tunnelStore)
	return true
}

func (b *e2bSandboxBackend) SetTimeout(ident, sandboxID string, seconds int32) bool {
	sb, ok := b.d.sandboxes.Get(sandboxID)
	if !ok || !b.owns(ident, sb) {
		return false
	}
	_ = b.d.sandboxes.Update(sandboxID, func(s *podpkg.SandboxInfo) {
		s.TTLSeconds = int64(seconds)
		s.LastActivity = time.Now()
	})
	return true
}

// PauseSandbox freezes the sandbox container (docker pause via the poder),
// preserving its state. Direct-agent sandboxes (a user's own machine) can't be
// paused. E2B's pause is a VM snapshot; SandrPod freezes in place, which keeps
// the sandbox ID valid for a later resume/connect.
func (b *e2bSandboxBackend) PauseSandbox(ident, sandboxID string) bool {
	sb, ok := b.d.sandboxes.Get(sandboxID)
	if !ok || !b.owns(ident, sb) || strings.HasPrefix(sb.ProxyURL, "direct://") {
		return false
	}
	if err := b.d.poderPodAction(sb, "stop"); err != nil {
		return false
	}
	b.d.sandboxes.Update(sandboxID, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStopped })
	return true
}

// ResumeSandbox unfreezes a paused sandbox and refreshes its idle timeout.
func (b *e2bSandboxBackend) ResumeSandbox(ident, sandboxID string, seconds int32) (e2bcompat.SandboxDetail, bool) {
	sb, ok := b.d.sandboxes.Get(sandboxID)
	if !ok || !b.owns(ident, sb) || strings.HasPrefix(sb.ProxyURL, "direct://") {
		return e2bcompat.SandboxDetail{}, false
	}
	if err := b.d.poderPodAction(sb, "start"); err != nil {
		return e2bcompat.SandboxDetail{}, false
	}
	b.d.sandboxes.Update(sandboxID, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateRunning })
	if seconds > 0 {
		b.SetTimeout(ident, sandboxID, seconds)
	}
	sb, _ = b.d.sandboxes.Get(sandboxID)
	return b.detailFromInfo(sb), true
}

// GetMetrics returns a single current resource sample for the sandbox, wrapped
// in the E2B list shape.
func (b *e2bSandboxBackend) GetMetrics(ident, sandboxID string) ([]e2bcompat.SandboxMetric, bool) {
	sb, ok := b.d.sandboxes.Get(sandboxID)
	if !ok || !b.owns(ident, sb) {
		return nil, false
	}
	var m struct {
		CPUCount   int     `json:"cpu_count"`
		CPUUsedPct float64 `json:"cpu_used_pct"`
		MemTotal   uint64  `json:"mem_total"`
		MemUsed    uint64  `json:"mem_used"`
		DiskTotal  uint64  `json:"disk_total"`
		DiskUsed   uint64  `json:"disk_used"`
	}
	if err := b.d.toolboxJSON(sandboxID, http.MethodGet, "metrics", nil, nil, &m); err != nil {
		return []e2bcompat.SandboxMetric{}, true // best-effort: empty on error
	}
	now := time.Now().UTC()
	return []e2bcompat.SandboxMetric{{
		CPUCount: m.CPUCount, CPUUsedPct: m.CPUUsedPct,
		MemTotal: m.MemTotal, MemUsed: m.MemUsed,
		DiskTotal: m.DiskTotal, DiskUsed: m.DiskUsed,
		Timestamp: now.Format(time.RFC3339), TimestampUnix: now.Unix(),
	}}, true
}

func (b *e2bSandboxBackend) owns(ident string, sb *podpkg.SandboxInfo) bool {
	return ident == "" || sb.Owner == "" || sb.Owner == ident
}

func (b *e2bSandboxBackend) detailFromInfo(sb *podpkg.SandboxInfo) e2bcompat.SandboxDetail {
	return e2bcompat.SandboxDetail{
		TemplateID: e2bTemplateFromLabels(sb.Labels), SandboxID: sb.Name, EnvdVersion: e2bcompat.DefaultEnvdVersion,
		StartedAt: sb.CreatedAt, CPUCount: 2, MemoryMB: 512, State: stateFor(sb),
		Metadata: e2bMetaFromLabels(sb.Labels),
		// The SDK authenticates envd calls with this token (Authorization:
		// Bearer). Return the sandbox's envd token so it round-trips and the
		// gateway's authenticator accepts the subsequent envd requests.
		EnvdAccessToken: b.d.envdToken(sb),
	}
}

// envdToken returns (creating if needed) a stable per-sandbox envd access
// token, stored in the sandbox labels so it survives and the authenticator can
// recognise it.
func (d e2bDeps) envdToken(sb *podpkg.SandboxInfo) string {
	if tok := sb.Labels[e2bEnvdTokenLabel]; tok != "" {
		return tok
	}
	tok, _ := e2bcompat.GenerateAPIKey()
	_ = d.sandboxes.Update(sb.Name, func(s *podpkg.SandboxInfo) {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[e2bEnvdTokenLabel] = tok
	})
	return tok
}

const e2bEnvdTokenLabel = "e2b.envd-token"

// E2B metadata + template are stored in SandboxInfo.Labels under a namespace so
// they don't collide with SandrPod's own labels.
const (
	e2bMetaPrefix    = "e2b.meta/"
	e2bTemplateLabel = "e2b.template"
)

func e2bMetaFromLabels(labels map[string]string) map[string]string {
	var out map[string]string
	for k, v := range labels {
		if suffix, ok := strings.CutPrefix(k, e2bMetaPrefix); ok {
			if out == nil {
				out = map[string]string{}
			}
			out[suffix] = v
		}
	}
	return out
}

func e2bTemplateFromLabels(labels map[string]string) string {
	if t := labels[e2bTemplateLabel]; t != "" {
		return t
	}
	return "base"
}

// metaMatches reports whether meta contains every key=value in filter.
func metaMatches(meta, filter map[string]string) bool {
	for k, v := range filter {
		if meta[k] != v {
			return false
		}
	}
	return true
}

func stateFor(sb *podpkg.SandboxInfo) e2bcompat.SandboxState {
	if sb.State == podpkg.StateStopped {
		return e2bcompat.StatePaused
	}
	return e2bcompat.StateRunning
}

// ─── EnvdBackend (toolbox proxy) ──────────────────────────────────────────────

type e2bEnvdBackend struct{ d e2bDeps }

func (b *e2bEnvdBackend) Stat(name, path string) (e2bcompat.EntryInfo, error) {
	var fi toolboxFileInfo
	if err := b.d.toolboxJSON(name, http.MethodGet, "files/info", url.Values{"path": {path}}, nil, &fi); err != nil {
		return e2bcompat.EntryInfo{}, err
	}
	return fi.toEntry(), nil
}

func (b *e2bEnvdBackend) ListDir(name, path string, _ uint32) ([]e2bcompat.EntryInfo, error) {
	var resp struct {
		Files []toolboxFileInfo `json:"files"`
	}
	if err := b.d.toolboxJSON(name, http.MethodGet, "files", url.Values{"path": {path}}, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]e2bcompat.EntryInfo, 0, len(resp.Files))
	for _, fi := range resp.Files {
		out = append(out, fi.toEntry())
	}
	return out, nil
}

func (b *e2bEnvdBackend) MakeDir(name, path string) (e2bcompat.EntryInfo, error) {
	if err := b.d.toolboxJSON(name, http.MethodPost, "files/folder", url.Values{"path": {path}}, nil, nil); err != nil {
		return e2bcompat.EntryInfo{}, err
	}
	return e2bcompat.EntryInfo{Name: baseName(path), Path: path, Type: e2bcompat.FileTypeDirectory}, nil
}

func (b *e2bEnvdBackend) Move(name, src, dst string) (e2bcompat.EntryInfo, error) {
	q := url.Values{"source": {src}, "destination": {dst}}
	if err := b.d.toolboxJSON(name, http.MethodPost, "files/move", q, nil, nil); err != nil {
		return e2bcompat.EntryInfo{}, err
	}
	return e2bcompat.EntryInfo{Name: baseName(dst), Path: dst, Type: e2bcompat.FileTypeFile}, nil
}

func (b *e2bEnvdBackend) Remove(name, path string) error {
	return b.d.toolboxJSON(name, http.MethodDelete, "files/delete", url.Values{"path": {path}}, nil, nil)
}

func (b *e2bEnvdBackend) ReadFile(name, path string) ([]byte, error) {
	resp, err := b.d.toolboxDo(name, http.MethodGet, "files/download", url.Values{"path": {path}}, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, e2bcompat.NotFoundError{Msg: "file not found"}
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("read %s: status %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (b *e2bEnvdBackend) WriteFile(name, path string, data []byte) (e2bcompat.EntryInfo, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", baseName(path))
	fw.Write(data)
	mw.Close()
	resp, err := b.d.toolboxDo(name, http.MethodPost, "files/upload", url.Values{"path": {path}}, &buf, mw.FormDataContentType())
	if err != nil {
		return e2bcompat.EntryInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return e2bcompat.EntryInfo{}, fmt.Errorf("write %s: %s", path, strings.TrimSpace(string(body)))
	}
	return e2bcompat.EntryInfo{Name: baseName(path), Path: path, Type: e2bcompat.FileTypeFile, Size: int64(len(data))}, nil
}

func (b *e2bEnvdBackend) StartProcess(name string, cfg e2bcompat.ProcessConfig) (e2bcompat.ProcResult, error) {
	// E2B commands.run sends an argv (often cmd="/bin/bash", args=["-l","-c",
	// "<script>"]). Extract the actual script after a "-c" flag; otherwise run
	// the reconstructed command line.
	code := cfg.Cmd
	if len(cfg.Args) > 0 {
		code = strings.TrimSpace(cfg.Cmd + " " + strings.Join(cfg.Args, " "))
		for i, a := range cfg.Args {
			if a == "-c" && i+1 < len(cfg.Args) {
				code = cfg.Args[i+1]
				break
			}
		}
	}
	reqBody := map[string]any{"language": "bash", "code": code, "timeout": 120}
	var res struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := b.d.toolboxJSON(name, http.MethodPost, "process", nil, reqBody, &res); err != nil {
		return e2bcompat.ProcResult{}, err
	}
	return e2bcompat.ProcResult{
		PID: 1, Stdout: []byte(res.Stdout), Stderr: []byte(res.Stderr), ExitCode: int32(res.ExitCode),
	}, nil
}

// ─── EnvdProcBackend: pid-addressed process table (toolbox /procmgr/*) ─────────

// StartProc spawns a process via the managed table and returns its live event
// stream. E2B's argv (e.g. /bin/bash -l -c "<script>") is run verbatim.
func (b *e2bEnvdBackend) StartProc(name string, cfg e2bcompat.ProcessConfig, pty *e2bcompat.PTYSize, stdin bool) (io.ReadCloser, error) {
	reqBody := map[string]any{"cmd": cfg.Cmd, "args": cfg.Args, "envs": cfg.Envs}
	if cfg.Cwd != nil {
		reqBody["cwd"] = *cfg.Cwd
	}
	if pty != nil {
		reqBody["pty"] = true
		reqBody["rows"] = pty.Rows
		reqBody["cols"] = pty.Cols
	}
	var sr struct {
		PID uint32 `json:"pid"`
	}
	if err := b.d.toolboxJSON(name, http.MethodPost, "procmgr/start", nil, reqBody, &sr); err != nil {
		return nil, err
	}
	return b.d.toolboxStream(name, http.MethodGet, "procmgr/stream", url.Values{"pid": {strconv.FormatUint(uint64(sr.PID), 10)}})
}

// ConnectProc re-attaches to a running process's output stream by pid.
func (b *e2bEnvdBackend) ConnectProc(name string, pid uint32) (io.ReadCloser, error) {
	return b.d.toolboxStream(name, http.MethodGet, "procmgr/stream", url.Values{"pid": {strconv.FormatUint(uint64(pid), 10)}})
}

// ListProcs returns the running processes.
func (b *e2bEnvdBackend) ListProcs(name string) ([]e2bcompat.ProcInfo, error) {
	var raw []struct {
		PID  uint32   `json:"pid"`
		Tag  string   `json:"tag"`
		Cmd  string   `json:"cmd"`
		Args []string `json:"args"`
		Cwd  string   `json:"cwd"`
	}
	if err := b.d.toolboxJSON(name, http.MethodGet, "procmgr/list", nil, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]e2bcompat.ProcInfo, 0, len(raw))
	for _, p := range raw {
		out = append(out, e2bcompat.ProcInfo{PID: p.PID, Tag: p.Tag, Cmd: p.Cmd, Args: p.Args, Cwd: p.Cwd})
	}
	return out, nil
}

// SendProcInput writes to a process's stdin (or PTY master when isPTY).
func (b *e2bEnvdBackend) SendProcInput(name string, pid uint32, data []byte, isPTY bool) error {
	return b.d.toolboxJSON(name, http.MethodPost, "procmgr/input", nil,
		map[string]any{"pid": pid, "data": data, "pty": isPTY}, nil)
}

// SignalProc sends a signal (kill) to a process.
func (b *e2bEnvdBackend) SignalProc(name string, pid uint32, signal int32) error {
	return b.d.toolboxJSON(name, http.MethodPost, "procmgr/signal", nil,
		map[string]any{"pid": pid, "signal": signal}, nil)
}

// ResizeProc resizes a PTY process's window.
func (b *e2bEnvdBackend) ResizeProc(name string, pid uint32, rows, cols uint32) error {
	return b.d.toolboxJSON(name, http.MethodPost, "procmgr/resize", nil,
		map[string]any{"pid": pid, "rows": rows, "cols": cols}, nil)
}

// CloseProcStdin closes a process's stdin.
func (b *e2bEnvdBackend) CloseProcStdin(name string, pid uint32) error {
	return b.d.toolboxJSON(name, http.MethodPost, "procmgr/stdin-close", nil,
		map[string]any{"pid": pid}, nil)
}

// ─── EnvdWatchBackend: directory watch (toolbox /watch/*) ──────────────────────

// CreateWatcher starts watching a directory and returns a watcher id.
func (b *e2bEnvdBackend) CreateWatcher(name, path string, recursive bool) (string, error) {
	var res struct {
		WatcherID string `json:"watcher_id"`
	}
	err := b.d.toolboxJSON(name, http.MethodPost, "watch/create", nil,
		map[string]any{"path": path, "recursive": recursive}, &res)
	return res.WatcherID, err
}

// GetWatcherEvents drains the events accrued for a watcher.
func (b *e2bEnvdBackend) GetWatcherEvents(name, watcherID string) ([]e2bcompat.WatchEvent, error) {
	var res struct {
		Events []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"events"`
	}
	if err := b.d.toolboxJSON(name, http.MethodGet, "watch/events",
		url.Values{"id": {watcherID}}, nil, &res); err != nil {
		return nil, err
	}
	out := make([]e2bcompat.WatchEvent, 0, len(res.Events))
	for _, ev := range res.Events {
		out = append(out, e2bcompat.WatchEvent{Name: ev.Name, Type: watchTypeToE2B(ev.Type)})
	}
	return out, nil
}

// RemoveWatcher stops a watcher.
func (b *e2bEnvdBackend) RemoveWatcher(name, watcherID string) error {
	return b.d.toolboxJSON(name, http.MethodPost, "watch/remove", nil,
		map[string]any{"watcher_id": watcherID}, nil)
}

// watchTypeToE2B maps the toolbox event-type name to the E2B EventType enum name.
func watchTypeToE2B(t string) string {
	switch t {
	case "create":
		return "EVENT_TYPE_CREATE"
	case "write":
		return "EVENT_TYPE_WRITE"
	case "remove":
		return "EVENT_TYPE_REMOVE"
	case "rename":
		return "EVENT_TYPE_RENAME"
	case "chmod":
		return "EVENT_TYPE_CHMOD"
	}
	return "EVENT_TYPE_UNSPECIFIED"
}

// toolboxFileInfo mirrors pkg/toolbox FileInfo ({name,path,is_dir,size}).
type toolboxFileInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

func (fi toolboxFileInfo) toEntry() e2bcompat.EntryInfo {
	t := e2bcompat.FileTypeFile
	if fi.IsDir {
		t = e2bcompat.FileTypeDirectory
	}
	return e2bcompat.EntryInfo{Name: fi.Name, Path: fi.Path, Type: t, Size: fi.Size}
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// randToken returns n hex bytes of randomness for a sandbox name.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is catastrophic and should never happen.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// statusRecorder captures the response status for debug logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

// headerNames lists request header keys for debug logging.
func headerNames(r *http.Request) []string {
	names := make([]string, 0, len(r.Header))
	for k := range r.Header {
		names = append(names, k)
	}
	return names
}
