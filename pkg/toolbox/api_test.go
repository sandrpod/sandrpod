// Copyright 2024 SandrPod
// HTTP handler tests for the Toolbox API server.

package toolbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer builds a Server whose executor.workDir points at a fresh temp
// directory so file-operation handlers act inside an isolated sandbox.
// Authentication is disabled (empty token).
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewServer("", "")
	// Override the executor's workDir so file handlers operate inside the temp dir.
	s.executor.workDir = dir
	return s, dir
}

// doRequest issues a request against the server's full Handler() (so middleware
// + routing are exercised) and returns the recorder.
func doRequest(t *testing.T, s *Server, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rdr)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// ---------- /health ----------

func TestHealthHandler_ReturnsJSONStatus(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/health", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp HealthStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Status != "ok" && resp.Status != "degraded" {
		t.Errorf("Status = %q, want ok|degraded", resp.Status)
	}
	if resp.Timestamp == 0 {
		t.Error("Timestamp should be non-zero")
	}
}

// /health is a public endpoint: even with a token set it must not require auth.
func TestHealthHandler_PublicEvenWithToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.token = "secret"
	rec := doRequest(t, s, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (health is public)", rec.Code)
	}
}

// ---------- auth middleware ----------

func TestAuthMiddleware_RejectsMissingToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.token = "secret"
	rec := doRequest(t, s, http.MethodGet, "/info", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_AcceptsValidToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.token = "secret"
	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with valid token", rec.Code)
	}
}

// ---------- /info ----------

func TestInfoHandler_ReturnsEnvironmentInfo(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/info", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var info EnvironmentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Arch == "" || info.OS == "" {
		t.Errorf("Arch/OS should be populated, got %+v", info)
	}
}

// ---------- /status ----------

func TestStatusHandler_ReportsRequestsAndExecutor(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["requests"]; !ok {
		t.Error("missing 'requests'")
	}
	if _, ok := body["uptime"]; !ok {
		t.Error("missing 'uptime'")
	}
	if _, ok := body["executor"]; !ok {
		t.Error("missing 'executor'")
	}
}

// ---------- /process ----------

func TestProcessHandler_WrongMethod_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/process", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestProcessHandler_InvalidJSON_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/process", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProcessHandler_BashEcho_ReturnsStdout(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/process",
		`{"language":"bash","code":"echo hello-toolbox"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var res ProcessResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello-toolbox") {
		t.Errorf("Stdout = %q, want to contain hello-toolbox", res.Stdout)
	}
}

func TestProcessHandler_UnsupportedLanguage_Returns500(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/process",
		`{"language":"brainfuck","code":"+++"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// ---------- /stream (SSE) ----------

func TestStreamHandler_WrongMethod_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodDelete, "/stream", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestStreamHandler_MissingParams_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/stream", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestStreamHandler_BashEcho_EmitsSSEEvents(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet,
		"/stream?language=bash&code="+"echo%20streamed", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: stdout") {
		t.Errorf("body missing 'event: stdout': %q", body)
	}
	if !strings.Contains(body, "streamed") {
		t.Errorf("body missing output 'streamed': %q", body)
	}
	if !strings.Contains(body, "event: exit") {
		t.Errorf("body missing 'event: exit': %q", body)
	}
}

func TestStreamHandler_POSTBody_Works(t *testing.T) {
	requireCmd(t, "bash")
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/stream",
		`{"language":"bash","code":"echo via-post"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "via-post") {
		t.Errorf("body missing 'via-post': %q", rec.Body.String())
	}
}

// ---------- /files/work-dir, /files/project-dir, /files/user-home-dir ----------

func TestWorkDirHandler_ReturnsPath(t *testing.T) {
	s, dir := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/work-dir", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["path"] != dir {
		t.Errorf("path = %q, want %q", body["path"], dir)
	}
}

func TestWorkDirHandler_WrongMethod_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/files/work-dir", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestProjectDirHandler_ReturnsPath(t *testing.T) {
	s, dir := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/project-dir", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["path"] != dir {
		t.Errorf("path = %q, want %q", body["path"], dir)
	}
}

func TestUserHomeDirHandler_ReturnsNonEmptyPath(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/user-home-dir", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["path"] == "" {
		t.Error("user home path should be non-empty")
	}
}

// ---------- /files (list) ----------

func TestFilesHandler_ListsDirectory(t *testing.T) {
	s, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodGet, "/files?path="+dir, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Path  string     `json:"path"`
		Files []FileInfo `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Files) != 1 || body.Files[0].Name != "a.txt" {
		t.Errorf("files = %+v, want [a.txt]", body.Files)
	}
}

func TestFilesHandler_EmptyPath_UsesProjectDir(t *testing.T) {
	s, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "z.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodGet, "/files", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestFilesHandler_WrongMethod_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/files", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// ---------- /files/info ----------

func TestFilesInfoHandler_ReturnsMetadata(t *testing.T) {
	s, dir := newTestServer(t)
	p := filepath.Join(dir, "info.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodGet, "/files/info?path="+p, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var info FileInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Name != "info.txt" || info.Size != 5 {
		t.Errorf("info = %+v, want name=info.txt size=5", info)
	}
}

func TestFilesInfoHandler_MissingPath_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/info", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFilesInfoHandler_NotFound_Returns404(t *testing.T) {
	s, dir := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/info?path="+filepath.Join(dir, "nope.txt"), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- /files/folder (create) ----------

func TestFilesFolderHandler_CreatesDir(t *testing.T) {
	s, dir := newTestServer(t)
	target := filepath.Join(dir, "newfolder")
	rec := doRequest(t, s, http.MethodPost, "/files/folder?path="+target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Errorf("folder not created: %v", err)
	}
}

func TestFilesFolderHandler_MissingPath_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/files/folder", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFilesFolderHandler_WrongMethod_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/folder", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// ---------- /files/delete ----------

func TestFilesDeleteHandler_RemovesFile(t *testing.T) {
	s, dir := newTestServer(t)
	p := filepath.Join(dir, "del.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodDelete, "/files/delete?path="+p, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("file should be deleted")
	}
}

func TestFilesDeleteHandler_MissingPath_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodDelete, "/files/delete", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFilesDeleteHandler_WrongMethod_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/delete", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// ---------- /files/move ----------

func TestFilesMoveHandler_RenamesFile(t *testing.T) {
	s, dir := newTestServer(t)
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodPost, "/files/move?source="+src+"&destination="+dst, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("destination not created: %v", err)
	}
}

func TestFilesMoveHandler_MissingParams_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/files/move?source=/a", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---------- /files/download ----------

func TestFilesDownloadHandler_StreamsContent(t *testing.T) {
	s, dir := newTestServer(t)
	p := filepath.Join(dir, "dl.txt")
	if err := os.WriteFile(p, []byte("download-me"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodGet, "/files/download?path="+p, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "download-me" {
		t.Errorf("body = %q, want download-me", rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "dl.txt") {
		t.Errorf("Content-Disposition = %q, want to contain dl.txt", cd)
	}
}

func TestFilesDownloadHandler_MissingPath_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/download", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFilesDownloadHandler_NotFound_Returns404(t *testing.T) {
	s, dir := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/download?path="+filepath.Join(dir, "missing.txt"), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFilesDownloadHandler_Directory_Returns400(t *testing.T) {
	s, dir := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/download?path="+dir, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for directory; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- /files/search ----------

func TestFilesSearchHandler_FindsMatch(t *testing.T) {
	s, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodGet, "/files/search?path="+dir+"&pattern=*.go", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFilesSearchHandler_MissingPattern_Returns400(t *testing.T) {
	s, dir := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/search?path="+dir, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---------- /files/find ----------

func TestFilesFindHandler_FindsPattern(t *testing.T) {
	s, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodGet, "/files/find?path="+dir+"&pattern=needle", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFilesFindHandler_MissingPattern_Returns400(t *testing.T) {
	s, dir := newTestServer(t)
	rec := doRequest(t, s, http.MethodGet, "/files/find?path="+dir, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---------- /files/replace ----------

func TestFilesReplaceHandler_ReplacesContent(t *testing.T) {
	s, dir := newTestServer(t)
	p := filepath.Join(dir, "r.txt")
	if err := os.WriteFile(p, []byte("foo foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := `{"files":["` + p + `"],"pattern":"foo","newValue":"bar"}`
	rec := doRequest(t, s, http.MethodPost, "/files/replace", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	content, _ := os.ReadFile(p)
	if strings.Contains(string(content), "foo") {
		t.Errorf("content still has foo: %q", content)
	}
}

func TestFilesReplaceHandler_InvalidJSON_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/files/replace", "{bad")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---------- /files/permissions ----------

func TestFilesPermissionsHandler_MissingPath_Returns400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, http.MethodPost, "/files/permissions", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFilesPermissionsHandler_SetsMode(t *testing.T) {
	s, dir := newTestServer(t)
	p := filepath.Join(dir, "perm.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, http.MethodPost, "/files/permissions?path="+p+"&mode=600", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

// ---------- access denied → 403 ----------

func TestFilesInfoHandler_BlacklistedPath_Returns403(t *testing.T) {
	s, _ := newTestServer(t)
	// /proc is on the read blacklist (Linux). On platforms without /proc the
	// path is still rejected because resolveSafePath checks the literal prefix.
	rec := doRequest(t, s, http.MethodGet, "/files/info?path=/proc/1/maps", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}
