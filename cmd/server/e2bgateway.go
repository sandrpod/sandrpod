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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
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
		Domain:    domain,
		Auth:      d.authenticator(),
		Sandboxes: &e2bSandboxBackend{d},
		Envd:      &e2bEnvdBackend{d},
		Code:      &e2bCodeBackend{d},
	})
}

// e2bCodeBackend implements the stateful run_code surface by proxying to the
// sandbox's toolbox /code-interpreter/execute endpoint.
type e2bCodeBackend struct{ d e2bDeps }

func (b *e2bCodeBackend) RunCode(name, contextID, code string) (e2bcompat.CodeExecution, error) {
	reqBody := map[string]string{"code": code, "context_id": contextID}
	var res struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Text   string `json:"text"`
		Error  string `json:"error"`
	}
	if err := b.d.toolboxJSON(name, http.MethodPost, "code-interpreter/execute", nil, reqBody, &res); err != nil {
		return e2bcompat.CodeExecution{}, err
	}
	return e2bcompat.CodeExecution{Stdout: res.Stdout, Stderr: res.Stderr, Text: res.Text, Error: res.Error}, nil
}

// authenticator maps a presented E2B key to a SandrPod identity. When auth is
// disabled it accepts any e2b_<hex>-shaped key anonymously.
func (d e2bDeps) authenticator() e2bcompat.Authenticator {
	return func(key string) (string, bool) {
		if key == "" {
			return "", false
		}
		authOff := d.cfg.Token == "" && len(d.cfg.Tokens) == 0 &&
			(d.cfg.Registry == nil || len(d.cfg.Registry.get()) == 0)
		if authOff {
			if e2bcompat.IsE2BKey(key) {
				return "", true // anonymous
			}
			return "", false
		}
		if id, ok := resolveToken(d.cfg, key); ok {
			return id.Name, true
		}
		return "", false
	}
}

// ─── toolbox call helper ──────────────────────────────────────────────────────

// toolboxDo performs an HTTP call to a sandbox's toolbox over the tunnel and
// returns the response. subpath is the toolbox path (e.g. "files/info").
func (d e2bDeps) toolboxDo(name, method, subpath string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	sb, ok := d.sandboxes.Get(name)
	if !ok {
		return nil, e2bcompat.NotFoundError{Msg: "sandbox not found"}
	}
	var (
		t      *tunnel.PoderTunnel
		target string
	)
	if strings.HasPrefix(sb.ProxyURL, "direct://") {
		dt, ok := d.directStore.Get(name)
		if !ok {
			return nil, e2bcompat.NotFoundError{Msg: "agent offline"}
		}
		t, target = dt, "http://agent/"+subpath
	} else {
		pt, ok := d.tunnelStore.Get(sb.PoderID)
		if !ok {
			return nil, e2bcompat.NotFoundError{Msg: "poder offline"}
		}
		t, target = pt, "http://poder/toolbox/"+name+"/"+subpath
	}
	if q := query.Encode(); q != "" {
		target += "?" + q
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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
	return resp, nil
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

func (b *e2bSandboxBackend) CreateSandbox(ident string, req e2bcompat.NewSandbox) (e2bcompat.SandboxDetail, error) {
	name := "e2b-" + randToken(8)
	create := &podpkg.CreateSandboxRequest{
		Name:         name,
		Region:       "local",
		ProviderType: "local",
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

// Pause/Resume are not yet backed (SandrPod has no snapshot-pause); report
// unsupported so the SDK surfaces a clear error rather than hanging.
func (b *e2bSandboxBackend) PauseSandbox(ident, sandboxID string) bool { return false }
func (b *e2bSandboxBackend) ResumeSandbox(ident, sandboxID string, _ int32) (e2bcompat.SandboxDetail, bool) {
	return e2bcompat.SandboxDetail{}, false
}

func (b *e2bSandboxBackend) owns(ident string, sb *podpkg.SandboxInfo) bool {
	return ident == "" || sb.Owner == "" || sb.Owner == ident
}

func (b *e2bSandboxBackend) detailFromInfo(sb *podpkg.SandboxInfo) e2bcompat.SandboxDetail {
	return e2bcompat.SandboxDetail{
		TemplateID: e2bTemplateFromLabels(sb.Labels), SandboxID: sb.Name, EnvdVersion: e2bcompat.DefaultEnvdVersion,
		StartedAt: sb.CreatedAt, CPUCount: 2, MemoryMB: 512, State: stateFor(sb),
		Metadata: e2bMetaFromLabels(sb.Labels),
	}
}

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
	var list []toolboxFileInfo
	if err := b.d.toolboxJSON(name, http.MethodGet, "files", url.Values{"path": {path}}, nil, &list); err != nil {
		return nil, err
	}
	out := make([]e2bcompat.EntryInfo, 0, len(list))
	for _, fi := range list {
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
	return b.d.toolboxJSON(name, http.MethodPost, "files/delete", url.Values{"path": {path}}, nil, nil)
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
	cmd := cfg.Cmd
	if len(cfg.Args) > 0 {
		cmd += " " + strings.Join(cfg.Args, " ")
	}
	reqBody := map[string]any{"language": "bash", "code": cmd, "timeout": 120}
	var res struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := b.d.toolboxJSON(name, http.MethodPost, "execute", nil, reqBody, &res); err != nil {
		return e2bcompat.ProcResult{}, err
	}
	return e2bcompat.ProcResult{
		PID: 1, Stdout: []byte(res.Stdout), Stderr: []byte(res.Stderr), ExitCode: int32(res.ExitCode),
	}, nil
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
