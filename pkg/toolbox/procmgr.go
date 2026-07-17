// Copyright 2026 SandrPod Contributors
// procmgr: a pid-addressed, background-capable process table. This backs the
// E2B Process service surface (Start / Connect / SendInput / SendSignal /
// Update / List / CloseStdin), which the e2bcompat gateway proxies to.
//
// Unlike the run-to-completion /process endpoint, a managed process keeps
// running after the starter disconnects (E2B background commands), can be
// re-attached to by pid (Connect), fed stdin (SendInput), resized (Update, for
// PTY), and killed (SendSignal). Output is buffered so a late subscriber
// replays the full history and then follows live until the process exits.

package toolbox

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// ProcEvent is one lifecycle/output event from a managed process. Data is
// base64-encoded in JSON (Go's default []byte marshaling), keeping it binary
// safe over the toolbox NDJSON stream.
type ProcEvent struct {
	Type     string `json:"type"` // "start" | "stdout" | "stderr" | "pty" | "end"
	PID      uint32 `json:"pid,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int32  `json:"exit_code"`
}

// ProcInfo is the metadata List returns for each running process.
type ProcInfo struct {
	PID  uint32            `json:"pid"`
	Tag  string            `json:"tag,omitempty"`
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

// ProcStartConfig configures a managed process.
type ProcStartConfig struct {
	Cmd  string
	Args []string
	Envs map[string]string
	Cwd  string
	Tag  string
	// PTY, when true, allocates a pseudo-terminal; output arrives as "pty"
	// events and SendInput with IsPTY writes to the master.
	PTY  bool
	Rows uint16
	Cols uint16
}

// managedProc is one entry in the table.
type managedProc struct {
	info  ProcInfo
	isPTY bool

	proc  *os.Process
	stdin io.WriteCloser
	ptmx  *os.File

	mu   sync.Mutex
	cond *sync.Cond
	buf  []ProcEvent // stdout/stderr/pty/end events (start is emitted separately)
	done bool
	exit int32
}

// append records an event and wakes any subscribers.
func (p *managedProc) append(ev ProcEvent) {
	p.mu.Lock()
	p.buf = append(p.buf, ev)
	if ev.Type == "end" {
		p.done = true
		p.exit = ev.ExitCode
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

// stream replays buffered events from the start, then follows live until the
// process exits. fn is called for each event; if it returns an error (client
// gone) streaming stops. Safe for multiple concurrent subscribers.
func (p *managedProc) stream(fn func(ProcEvent) error) error {
	p.mu.Lock()
	cursor := 0
	for {
		for cursor < len(p.buf) {
			ev := p.buf[cursor]
			cursor++
			p.mu.Unlock()
			if err := fn(ev); err != nil {
				return err
			}
			p.mu.Lock()
		}
		if p.done && cursor >= len(p.buf) {
			p.mu.Unlock()
			return nil
		}
		p.cond.Wait()
	}
}

// ProcManager is the process table.
type ProcManager struct {
	mu    sync.Mutex
	procs map[uint32]*managedProc
}

// NewProcManager builds an empty table.
func NewProcManager() *ProcManager {
	return &ProcManager{procs: map[uint32]*managedProc{}}
}

// Start spawns a process and registers it. It returns the OS pid immediately;
// the process runs in the background until it exits or is signalled.
func (m *ProcManager) Start(cfg ProcStartConfig) (uint32, error) {
	if cfg.Cmd == "" {
		cfg.Cmd = nativeShell()
	}
	cwd := cfg.Cwd
	if cwd == "" {
		cwd = defaultWorkDir()
	}
	cmd := exec.Command(cfg.Cmd, cfg.Args...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(cfg.Envs)

	mp := &managedProc{
		isPTY: cfg.PTY,
		info: ProcInfo{
			Tag: cfg.Tag, Cmd: cfg.Cmd, Args: cfg.Args, Envs: cfg.Envs, Cwd: cwd,
		},
	}
	mp.cond = sync.NewCond(&mp.mu)

	if cfg.PTY {
		// startPTY calls cmd.Start internally; output+input share the master.
		ptmx, err := startPTY(cmd, cfg.Rows, cfg.Cols)
		if err != nil {
			return 0, err
		}
		mp.ptmx = ptmx
		mp.stdin = ptmx
		mp.proc = cmd.Process
		mp.info.PID = uint32(cmd.Process.Pid)
		m.register(mp)
		go func() {
			pumpReader(mp, ptmx, "pty") // returns on master EOF (process gone)
			mp.append(ProcEvent{Type: "end", ExitCode: waitExit(cmd)})
		}()
		return mp.info.PID, nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	mp.stdin = stdin
	mp.proc = cmd.Process
	mp.info.PID = uint32(cmd.Process.Pid)
	m.register(mp)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pumpReader(mp, stdout, "stdout") }()
	go func() { defer wg.Done(); pumpReader(mp, stderr, "stderr") }()
	go func() {
		wg.Wait() // drain both pipes before recording the exit
		mp.append(ProcEvent{Type: "end", ExitCode: waitExit(cmd)})
	}()
	return mp.info.PID, nil
}

func (m *ProcManager) register(mp *managedProc) {
	m.mu.Lock()
	m.procs[mp.info.PID] = mp
	m.mu.Unlock()
}

// get returns a process by pid.
func (m *ProcManager) get(pid uint32) (*managedProc, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, ok := m.procs[pid]
	return mp, ok
}

// List returns metadata for every process still in the table (running or
// recently exited but not yet reaped).
func (m *ProcManager) List() []ProcInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ProcInfo, 0, len(m.procs))
	for _, mp := range m.procs {
		mp.mu.Lock()
		if !mp.done {
			out = append(out, mp.info)
		}
		mp.mu.Unlock()
	}
	return out
}

// Stream replays and follows a process's output. Returns false if unknown.
// The first event delivered is a synthetic "start" so re-attaching clients see
// the pid.
func (m *ProcManager) Stream(pid uint32, fn func(ProcEvent) error) (bool, error) {
	mp, ok := m.get(pid)
	if !ok {
		return false, nil
	}
	if err := fn(ProcEvent{Type: "start", PID: pid}); err != nil {
		return true, err
	}
	return true, mp.stream(fn)
}

// SendInput writes to a process's stdin (or PTY master when isPTY).
func (m *ProcManager) SendInput(pid uint32, data []byte, isPTY bool) bool {
	mp, ok := m.get(pid)
	if !ok || mp.stdin == nil {
		return false
	}
	_, _ = mp.stdin.Write(data)
	return true
}

// CloseStdin closes a process's stdin.
func (m *ProcManager) CloseStdin(pid uint32) bool {
	mp, ok := m.get(pid)
	if !ok || mp.stdin == nil {
		return false
	}
	_ = mp.stdin.Close()
	return true
}

// Signal sends a signal to a process. E2B only uses SIGKILL/SIGTERM.
func (m *ProcManager) Signal(pid uint32, sig syscall.Signal) bool {
	mp, ok := m.get(pid)
	if !ok || mp.proc == nil {
		return false
	}
	_ = mp.proc.Signal(sig)
	return true
}

// Resize changes a PTY process's window size. No-op (false) for non-PTY.
func (m *ProcManager) Resize(pid uint32, rows, cols uint16) bool {
	mp, ok := m.get(pid)
	if !ok || mp.ptmx == nil {
		return false
	}
	return resizePTY(mp.ptmx, rows, cols) == nil
}

// pumpReader copies a reader into the process's event buffer in chunks.
func pumpReader(mp *managedProc, r io.Reader, kind string) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			mp.append(ProcEvent{Type: kind, Data: cp})
		}
		if err != nil {
			return
		}
	}
}

// mergeEnv overlays the caller-supplied envs on top of the toolbox environment.
func mergeEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// waitExit waits for a command and returns its exit code (-1 on signal/error).
func waitExit(cmd *exec.Cmd) int32 {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return int32(ee.ExitCode())
	}
	return -1
}
