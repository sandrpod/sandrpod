package e2bcompat

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- API key ----

func TestAPIKey(t *testing.T) {
	if !IsE2BKey("e2b_0123456789abcdef") {
		t.Error("valid e2b key rejected")
	}
	if IsE2BKey("e2b_XYZ") || IsE2BKey("sk-123") || IsE2BKey("") {
		t.Error("invalid key accepted")
	}
	k, err := GenerateAPIKey()
	if err != nil || !IsE2BKey(k) {
		t.Fatalf("generated key invalid: %q %v", k, err)
	}
	if got := TokenFromKey("e2b_deadbeef"); got != "deadbeef" {
		t.Errorf("TokenFromKey = %q", got)
	}
}

// ---- fakes ----

type fakeSandboxes struct{ created map[string]SandboxDetail }

func newFakeSandboxes() *fakeSandboxes { return &fakeSandboxes{created: map[string]SandboxDetail{}} }

func (f *fakeSandboxes) CreateSandbox(ident string, req NewSandbox) (SandboxDetail, error) {
	d := SandboxDetail{
		TemplateID: req.TemplateID, SandboxID: "sbx-" + fmt.Sprint(len(f.created)+1),
		EnvdVersion: DefaultEnvdVersion, State: StateRunning, CPUCount: 2, MemoryMB: 512,
		Metadata: req.Metadata,
	}
	f.created[d.SandboxID] = d
	return d, nil
}
func (f *fakeSandboxes) GetSandbox(_, id string) (SandboxDetail, bool) {
	d, ok := f.created[id]
	return d, ok
}
func (f *fakeSandboxes) ListSandboxes(_ string, _ map[string]string) []ListedSandbox {
	var out []ListedSandbox
	for _, d := range f.created {
		out = append(out, ListedSandbox{SandboxID: d.SandboxID, TemplateID: d.TemplateID, State: d.State, EnvdVersion: d.EnvdVersion})
	}
	return out
}
func (f *fakeSandboxes) KillSandbox(_, id string) bool {
	if _, ok := f.created[id]; !ok {
		return false
	}
	delete(f.created, id)
	return true
}
func (f *fakeSandboxes) SetTimeout(_, id string, _ int32) bool { _, ok := f.created[id]; return ok }
func (f *fakeSandboxes) PauseSandbox(_, id string) bool        { _, ok := f.created[id]; return ok }
func (f *fakeSandboxes) ResumeSandbox(_, id string, _ int32) (SandboxDetail, bool) {
	d, ok := f.created[id]
	return d, ok
}

type fakeEnvd struct{ files map[string][]byte }

func newFakeEnvd() *fakeEnvd { return &fakeEnvd{files: map[string][]byte{}} }

func (f *fakeEnvd) Stat(_, path string) (EntryInfo, error) {
	if _, ok := f.files[path]; !ok {
		return EntryInfo{}, NotFoundError{Msg: "no such file"}
	}
	return EntryInfo{Name: path, Path: path, Type: FileTypeFile, Size: int64(len(f.files[path]))}, nil
}
func (f *fakeEnvd) MakeDir(_, path string) (EntryInfo, error) {
	return EntryInfo{Name: path, Path: path, Type: FileTypeDirectory}, nil
}
func (f *fakeEnvd) Move(_, src, dst string) (EntryInfo, error) {
	f.files[dst] = f.files[src]
	delete(f.files, src)
	return EntryInfo{Name: dst, Path: dst, Type: FileTypeFile}, nil
}
func (f *fakeEnvd) ListDir(_, path string, _ uint32) ([]EntryInfo, error) {
	var out []EntryInfo
	for p := range f.files {
		if strings.HasPrefix(p, path) {
			out = append(out, EntryInfo{Name: p, Path: p, Type: FileTypeFile})
		}
	}
	return out, nil
}
func (f *fakeEnvd) Remove(_, path string) error { delete(f.files, path); return nil }
func (f *fakeEnvd) ReadFile(_, path string) ([]byte, error) {
	d, ok := f.files[path]
	if !ok {
		return nil, NotFoundError{}
	}
	return d, nil
}
func (f *fakeEnvd) WriteFile(_, path string, data []byte) (EntryInfo, error) {
	f.files[path] = data
	return EntryInfo{Name: path, Path: path, Type: FileTypeFile, Size: int64(len(data))}, nil
}
func (f *fakeEnvd) StartProcess(_ string, cfg ProcessConfig) (ProcResult, error) {
	return ProcResult{PID: 42, Stdout: []byte("hello from " + cfg.Cmd), ExitCode: 0}, nil
}

type fakeCode struct{ state map[string]int }

func (f *fakeCode) RunCode(_, ctx, code string) (CodeExecution, error) {
	// Minimal stateful stand-in: "inc" bumps a per-context counter, "get"
	// returns it, "boom" errors, anything else echoes to stdout.
	if f.state == nil {
		f.state = map[string]int{}
	}
	switch code {
	case "inc":
		f.state[ctx]++
		return CodeExecution{}, nil
	case "get":
		return CodeExecution{Text: fmt.Sprint(f.state[ctx])}, nil
	case "boom":
		return CodeExecution{Error: "Traceback...\nZeroDivisionError: division by zero"}, nil
	default:
		return CodeExecution{Stdout: code + "\n"}, nil
	}
}

func (f *fakeCode) CreateContext(_, language, cwd string) (CodeContext, error) {
	return CodeContext{ID: "ctx1", Language: language, Cwd: cwd}, nil
}
func (f *fakeCode) ListContexts(_ string) ([]CodeContext, error) {
	return []CodeContext{{ID: "ctx1", Language: "python"}}, nil
}
func (f *fakeCode) RemoveContext(_, _ string) error  { return nil }
func (f *fakeCode) RestartContext(_, _ string) error { return nil }

func testGateway() http.Handler {
	return Handler(Config{
		Auth:      func(k string) (string, bool) { return "user1", IsE2BKey(k) },
		Sandboxes: newFakeSandboxes(),
		Envd:      newFakeEnvd(),
		Code:      &fakeCode{},
	})
}

func do(t *testing.T, h http.Handler, method, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-API-KEY", "e2b_"+strings.Repeat("a", 40))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- control plane ----

func TestControlPlane_Lifecycle(t *testing.T) {
	h := testGateway()

	// Unauthenticated → 401.
	req := httptest.NewRequest("GET", "/sandboxes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", rec.Code)
	}

	// Create.
	rec = do(t, h, "POST", "/sandboxes", NewSandbox{TemplateID: "base", Timeout: 60}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body)
	}
	var sb Sandbox
	json.Unmarshal(rec.Body.Bytes(), &sb)
	if sb.SandboxID == "" || sb.TemplateID != "base" || sb.EnvdVersion == "" {
		t.Fatalf("create response missing fields: %+v", sb)
	}

	// Get.
	rec = do(t, h, "GET", "/sandboxes/"+sb.SandboxID, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get: %d", rec.Code)
	}
	var detail SandboxDetail
	json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.SandboxID != sb.SandboxID || detail.State != StateRunning {
		t.Errorf("get detail wrong: %+v", detail)
	}

	// List.
	rec = do(t, h, "GET", "/sandboxes", nil, nil)
	var list []ListedSandbox
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Errorf("list: want 1, got %d", len(list))
	}

	// Set timeout → 204.
	rec = do(t, h, "POST", "/sandboxes/"+sb.SandboxID+"/timeout", SetTimeoutRequest{Timeout: 30}, nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("timeout: want 204, got %d", rec.Code)
	}

	// Kill → 204, then get → 404.
	rec = do(t, h, "DELETE", "/sandboxes/"+sb.SandboxID, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("kill: want 204, got %d", rec.Code)
	}
	rec = do(t, h, "GET", "/sandboxes/"+sb.SandboxID, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("get after kill: want 404, got %d", rec.Code)
	}
}

// ---- envd filesystem + files ----

func TestEnvd_FilesystemAndFiles(t *testing.T) {
	h := testGateway()
	sid := map[string]string{"X-Sandbox-ID": "sbx-1"}

	// Write file (HTTP).
	req := httptest.NewRequest("POST", "/files?path=/tmp/a.txt", bytes.NewReader([]byte("data123")))
	req.Header.Set("X-API-KEY", "e2b_"+strings.Repeat("a", 40))
	req.Header.Set("X-Sandbox-ID", "sbx-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("write file: %d %s", rec.Code, rec.Body)
	}

	// Read file back.
	req = httptest.NewRequest("GET", "/files?path=/tmp/a.txt", nil)
	req.Header.Set("X-API-KEY", "e2b_"+strings.Repeat("a", 40))
	req.Header.Set("X-Sandbox-ID", "sbx-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "data123" {
		t.Fatalf("read file: %d %q", rec.Code, rec.Body.String())
	}

	// Stat via connect.
	rec = do(t, h, "POST", "/filesystem.Filesystem/Stat", statReq{Path: "/tmp/a.txt"}, sid)
	if rec.Code != 200 {
		t.Fatalf("stat: %d %s", rec.Code, rec.Body)
	}
	var sr statResp
	json.Unmarshal(rec.Body.Bytes(), &sr)
	if sr.Entry.Type != FileTypeFile || sr.Entry.Size != 7 {
		t.Errorf("stat entry wrong: %+v", sr.Entry)
	}

	// Stat missing → connect not_found.
	rec = do(t, h, "POST", "/filesystem.Filesystem/Stat", statReq{Path: "/nope"}, sid)
	if rec.Code != http.StatusNotFound {
		t.Errorf("stat missing: want 404, got %d", rec.Code)
	}
	var cerr map[string]string
	json.Unmarshal(rec.Body.Bytes(), &cerr)
	if cerr["code"] != "not_found" {
		t.Errorf("connect error code = %q", cerr["code"])
	}

	// ListDir.
	rec = do(t, h, "POST", "/filesystem.Filesystem/ListDir", listDirReq{Path: "/tmp", Depth: 1}, sid)
	var ld listDirResp
	json.Unmarshal(rec.Body.Bytes(), &ld)
	if len(ld.Entries) != 1 {
		t.Errorf("listdir: %+v", ld.Entries)
	}

	// Remove.
	rec = do(t, h, "POST", "/filesystem.Filesystem/Remove", removeReq{Path: "/tmp/a.txt"}, sid)
	if rec.Code != 200 {
		t.Errorf("remove: %d", rec.Code)
	}
}

// ---- envd process Start (connect server-stream) ----

func TestEnvd_ProcessStartStream(t *testing.T) {
	h := testGateway()
	rec := do(t, h, "POST", "/process.Process/Start",
		startReq{Process: ProcessConfig{Cmd: "echo"}},
		map[string]string{"X-Sandbox-ID": "sbx-1"})
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/connect+json" {
		t.Errorf("content-type = %q", ct)
	}
	frames := parseConnectFrames(t, rec.Body.Bytes())
	if len(frames) < 3 {
		t.Fatalf("want >=3 frames (start,data,end + eos), got %d: %v", len(frames), frames)
	}
	// First frame is a start event with a pid.
	if _, ok := frames[0]["event"].(map[string]any)["start"]; !ok {
		t.Errorf("first frame not a start event: %v", frames[0])
	}
	// The last non-eos frame must be an end event with exitCode 0.
	last := frames[len(frames)-1]
	end, _ := last["event"].(map[string]any)["end"].(map[string]any)
	if end == nil || end["exitCode"].(float64) != 0 {
		t.Errorf("missing/invalid end event: %v", last)
	}
}

// ---- code interpreter (run_code) ----

func TestCodeInterpreter_RunCode(t *testing.T) {
	h := testGateway()
	sid := map[string]string{"X-Sandbox-ID": "sbx-1"}

	// Stateful: inc twice, then get → "2" as a result message.
	do(t, h, "POST", "/execute", map[string]string{"code": "inc", "context_id": "c"}, sid)
	do(t, h, "POST", "/execute", map[string]string{"code": "inc", "context_id": "c"}, sid)
	rec := do(t, h, "POST", "/execute", map[string]string{"code": "get", "context_id": "c"}, sid)
	if rec.Code != 200 {
		t.Fatalf("run_code: %d %s", rec.Code, rec.Body)
	}
	msgs := parseNDJSON(t, rec.Body.Bytes())
	var gotResult bool
	for _, m := range msgs {
		if m["type"] == "result" && m["text"] == "2" && m["is_main_result"] == true {
			gotResult = true
		}
	}
	if !gotResult {
		t.Errorf("expected result text=2, got %v", msgs)
	}
	if msgs[len(msgs)-1]["type"] != "end_of_execution" {
		t.Errorf("stream must end with end_of_execution: %v", msgs[len(msgs)-1])
	}

	// Error path → an error message, no result.
	rec = do(t, h, "POST", "/execute", map[string]string{"code": "boom"}, sid)
	msgs = parseNDJSON(t, rec.Body.Bytes())
	var gotErr bool
	for _, m := range msgs {
		if m["type"] == "error" && m["value"] == "ZeroDivisionError: division by zero" {
			gotErr = true
		}
	}
	if !gotErr {
		t.Errorf("expected error message with the exception summary, got %v", msgs)
	}
}

func parseNDJSON(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad ndjson line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// parseConnectFrames decodes the 5-byte-prefixed connect stream, skipping the
// terminal end-of-stream frame (flag 0x02).
func parseConnectFrames(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for len(b) >= 5 {
		flag := b[0]
		n := binary.BigEndian.Uint32(b[1:5])
		payload := b[5 : 5+n]
		b = b[5+n:]
		if flag&0x02 != 0 {
			break // end-of-stream
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("bad frame json: %v", err)
		}
		out = append(out, m)
	}
	return out
}
