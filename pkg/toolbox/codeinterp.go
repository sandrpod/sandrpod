// Copyright 2024 SandrPod
// Stateful code interpreter for E2B-compatible run_code. Each "context" is a
// long-lived Python process holding a persistent global namespace, so variables
// survive across executions (x=1 then x+=1;x → 2). Output (stdout/stderr) and
// the value of the final expression are captured and returned.
//
// This provides the core stateful-run_code semantics without a full Jupyter
// kernel. Rich display results (matplotlib figures, HTML) require the jupyter
// kernel bundled in the toolbox image; see the `results` field which is wired
// for that and populated when the driver emits display data.

package toolbox

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// pyDriver is a persistent REPL: it reads one JSON request per line
// ({"code": "..."} base64) from stdin, execs it in a persistent namespace,
// captures stdout/stderr and the last expression's repr, and writes one JSON
// response line. Kept dependency-free (stdlib only) so it runs on any python3.
const pyDriver = `
import sys, io, json, base64, ast, traceback
_ns = {"__name__": "__main__"}
def _jsonable(o):
    # json default for values json can't handle: numpy scalars → native via
    # .item(), numpy arrays → list via .tolist(), everything else → str.
    if hasattr(o, "item"):
        try: return o.item()
        except Exception: pass
    if hasattr(o, "tolist"):
        try: return o.tolist()
        except Exception: pass
    return str(o)
# IPython-style rich reprs → E2B Result MIME keys. This is how the official E2B
# kernel formats results (DataFrames yield html, sympy yields latex, etc.).
_MIME = (("_repr_html_","html"),("_repr_markdown_","markdown"),("_repr_svg_","svg"),
         ("_repr_latex_","latex"),("_repr_json_","json"),("_repr_javascript_","javascript"),
         ("_repr_jpeg_","jpeg"),("_repr_png_","png"),("_repr_pdf_","pdf"))
def _mime_result(val, is_main):
    r = {"is_main_result": is_main, "text": repr(val)}
    for method, key in _MIME:
        fn = getattr(val, method, None)
        if not callable(fn):
            continue
        try:
            data = fn()
        except Exception:
            continue
        if data is None:
            continue
        if isinstance(data, tuple):  # some reprs return (data, metadata)
            data = data[0]
        if key in ("png", "jpeg", "pdf") and isinstance(data, (bytes, bytearray)):
            data = base64.b64encode(data).decode("ascii")
        r[key] = data
    return r
def _chart_axes(ax):
    # Parse one matplotlib Axes into E2B's structured chart schema.
    from matplotlib.patches import Wedge, Rectangle
    from matplotlib.collections import PathCollection
    title = ax.get_title() or None
    wedges = [p for p in ax.patches if isinstance(p, Wedge)]
    if wedges:
        texts = [t.get_text() for t in ax.texts]
        elems = [{"label": (texts[i] if i < len(texts) else ""),
                  "angle": float(abs(w.theta2 - w.theta1)), "radius": float(w.r)} for i, w in enumerate(wedges)]
        return {"type": "pie", "title": title, "elements": elems}
    def _ticks(getter, lblgetter):
        try: t = [float(v) for v in getter()]
        except Exception: t = []
        return t, [l.get_text() for l in lblgetter()]
    xt, xtl = _ticks(ax.get_xticks, ax.get_xticklabels)
    yt, ytl = _ticks(ax.get_yticks, ax.get_yticklabels)
    base = {"title": title, "x_label": ax.get_xlabel() or None, "y_label": ax.get_ylabel() or None,
            "x_unit": None, "y_unit": None, "x_scale": ax.get_xscale(), "y_scale": ax.get_yscale(),
            "x_ticks": xt, "x_tick_labels": xtl, "y_ticks": yt, "y_tick_labels": ytl}
    lines = ax.get_lines()
    scatters = [c for c in ax.collections if isinstance(c, PathCollection)]
    bars = [p for p in ax.patches if isinstance(p, Rectangle) and p.get_width() and p.get_height()]
    if bars and not lines and not scatters:
        xl = [l.get_text() for l in ax.get_xticklabels()]
        elems = [{"label": (xl[i] if i < len(xl) else str(i)), "value": float(b.get_height()), "group": None}
                 for i, b in enumerate(bars)]
        return {"type": "bar", "elements": elems, **base}
    if scatters and not lines:
        elems = [{"label": ("" if sc.get_label().startswith("_") else sc.get_label()),
                  "points": [[float(x), float(y)] for x, y in sc.get_offsets()]} for sc in scatters]
        return {"type": "scatter", "elements": elems, **base}
    if lines:
        all_scatter = all(str(ln.get_linestyle()) in ("None", "") and str(ln.get_marker()) not in ("None", "") for ln in lines)
        elems = [{"label": ("" if ln.get_label().startswith("_") else ln.get_label()),
                  "points": [[float(x), float(y)] for x, y in zip(ln.get_xdata(), ln.get_ydata())]} for ln in lines]
        return {"type": ("scatter" if all_scatter else "line"), "elements": elems, **base}
    return {"type": "unknown", "elements": [], **base}
def _extract_chart(fig):
    axes = fig.get_axes()
    if not axes:
        return None
    if len(axes) > 1:
        return {"type": "superchart", "title": (fig._suptitle.get_text() if fig._suptitle else None),
                "elements": [_chart_axes(a) for a in axes]}
    return _chart_axes(axes[0])
def _figures():
    # Open matplotlib figures become PNG display results (E2B-style) plus a
    # structured chart object parsed from the axes, then close.
    plt = sys.modules.get("matplotlib.pyplot")
    if plt is None:
        return []
    figs = []
    try:
        for num in plt.get_fignums():
            fig = plt.figure(num)
            buf = io.BytesIO()
            fig.savefig(buf, format="png", bbox_inches="tight")
            item = {"is_main_result": False, "png": base64.b64encode(buf.getvalue()).decode("ascii")}
            try:
                ch = _extract_chart(fig)
                if ch:
                    item["chart"] = ch
            except Exception:
                pass
            figs.append(item)
        plt.close("all")
    except Exception:
        pass
    return figs
def _run(src):
    out, err, error, val, has = io.StringIO(), io.StringIO(), None, None, False
    so, se = sys.stdout, sys.stderr
    sys.stdout, sys.stderr = out, err
    try:
        block = ast.parse(src, mode="exec")
        last = None
        if block.body and isinstance(block.body[-1], ast.Expr):
            last = ast.Expression(block.body.pop().value)
        exec(compile(block, "<cell>", "exec"), _ns)
        if last is not None:
            val = eval(compile(last, "<cell>", "eval"), _ns)
            has = val is not None
    except Exception:
        error = traceback.format_exc()
    finally:
        sys.stdout, sys.stderr = so, se
    results = _figures()
    if has:
        results.append(_mime_result(val, True))
    # Convenience mirrors for simple consumers (CLI/console text + images).
    text, images = "", []
    for r in results:
        if r.get("is_main_result"):
            text = r.get("text") or ""
        if r.get("png"):
            images.append(r["png"])
    return {"stdout": out.getvalue(), "stderr": err.getvalue(), "error": error,
            "text": text, "images": images, "results": results}
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        src = base64.b64decode(req["code"]).decode("utf-8")
    except Exception as e:
        sys.stdout.write(json.dumps({"stdout":"","stderr":"","text":None,"error":str(e)}) + "\n")
        sys.stdout.flush(); continue
    # default= makes numpy scalars/arrays serializable; the try means a bad
    # result never kills the kernel (which would break the whole context).
    try:
        payload = json.dumps(_run(src), default=_jsonable)
    except Exception as e:
        payload = json.dumps({"stdout":"","stderr":"","text":None,"error":"serialize: "+str(e)})
    sys.stdout.write(payload + "\n")
    sys.stdout.flush()
`

// CodeResult is one run_code outcome.
type CodeResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Text   string `json:"text"`  // main result's text/plain (convenience mirror)
	Error  string `json:"error"` // traceback, if the cell raised
	// Images holds base64-encoded PNGs (convenience mirror of Results' png).
	Images []string `json:"images,omitempty"`
	// Results is the E2B-shaped rich result list: matplotlib figures plus the
	// final expression, each carrying its MIME reprs (text/html/svg/png/latex/…).
	Results []map[string]any `json:"results,omitempty"`
}

// kernel is one persistent interpreter process.
type kernel struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// ContextInfo is an E2B code-interpreter context.
type ContextInfo struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Cwd      string `json:"cwd"`
}

// KernelManager owns per-context kernels.
type KernelManager struct {
	mu       sync.Mutex
	kernels  map[string]*kernel
	contexts map[string]ContextInfo
	python   string // python3 executable
}

// NewKernelManager returns a manager using the given python3 executable
// (defaults to "python3").
func NewKernelManager(python string) *KernelManager {
	if python == "" {
		python = "python3"
	}
	return &KernelManager{kernels: map[string]*kernel{}, contexts: map[string]ContextInfo{}, python: python}
}

// CreateContext registers a new context and returns its id. The kernel starts
// lazily on the first Execute.
func (m *KernelManager) CreateContext(language, cwd string) ContextInfo {
	if language == "" {
		language = "python"
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	ci := ContextInfo{ID: hex.EncodeToString(b), Language: language, Cwd: cwd}
	m.mu.Lock()
	m.contexts[ci.ID] = ci
	m.mu.Unlock()
	return ci
}

// ListContexts returns the registered contexts.
func (m *KernelManager) ListContexts() []ContextInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ContextInfo, 0, len(m.contexts))
	for _, ci := range m.contexts {
		out = append(out, ci)
	}
	return out
}

// Restart tears down a context's kernel but keeps the context registered; the
// next Execute starts a fresh kernel (clean namespace).
func (m *KernelManager) Restart(contextID string) {
	m.mu.Lock()
	k, ok := m.kernels[contextID]
	delete(m.kernels, contextID)
	m.mu.Unlock()
	if ok {
		_ = k.stdin.Close()
		_ = k.cmd.Process.Kill()
		_ = k.cmd.Wait()
	}
}

func (m *KernelManager) get(contextID string) (*kernel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.kernels[contextID]; ok {
		return k, nil
	}
	cmd := exec.Command(m.python, "-c", pyDriver)
	// Force matplotlib's non-interactive Agg backend so plt.show() is a no-op
	// headless and figures stay in memory for us to capture as PNG.
	cmd.Env = append(os.Environ(), "MPLBACKEND=Agg")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start python kernel: %w", err)
	}
	k := &kernel{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	m.kernels[contextID] = k
	return k, nil
}

// Execute runs code in the context's persistent kernel.
func (m *KernelManager) Execute(contextID, code string) (CodeResult, error) {
	k, err := m.get(contextID)
	if err != nil {
		return CodeResult{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	req, _ := json.Marshal(map[string]string{"code": base64.StdEncoding.EncodeToString([]byte(code))})
	if _, err := k.stdin.Write(append(req, '\n')); err != nil {
		return CodeResult{}, fmt.Errorf("write to kernel: %w", err)
	}
	line, err := k.stdout.ReadBytes('\n')
	if err != nil {
		return CodeResult{}, fmt.Errorf("read from kernel: %w", err)
	}
	var res CodeResult
	if err := json.Unmarshal(line, &res); err != nil {
		return CodeResult{}, fmt.Errorf("decode kernel result: %w", err)
	}
	return res, nil
}

// Close tears down a context's kernel and unregisters it (idempotent).
func (m *KernelManager) Close(contextID string) {
	m.mu.Lock()
	k, ok := m.kernels[contextID]
	delete(m.kernels, contextID)
	delete(m.contexts, contextID)
	m.mu.Unlock()
	if ok {
		_ = k.stdin.Close()
		_ = k.cmd.Process.Kill()
		_ = k.cmd.Wait()
	}
}

// CloseAll tears down every kernel.
func (m *KernelManager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.kernels))
	for id := range m.kernels {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Close(id)
	}
}
