//go:build windows

// Copyright 2024 SandrPod
// PTY Session Manager for Windows, backed by ConPTY (Windows 10 1809+).
//
// This mirrors pty_unix.go's external surface (PtySession / PtyManager /
// PtyHandler with CreateSession, GetSession, CloseSession, ResizeSession,
// ListSessions, HandleWebSocket) so api.go does not need any build tags.
//
// Implementation notes:
//
//   - ConPTY is a Win10 1809+ kernel feature; CreatePseudoConsole returns
//     E_NOTIMPL on older builds. We surface that as a regular CreateSession
//     error so the operator sees a clean "Windows 10 1809+ required" message
//     instead of a cryptic syscall failure.
//
//   - We use *os.File on top of pipe handles for both directions because the
//     existing handler code wants io.Reader / io.Writer semantics and Go's
//     runtime poller can sit on the underlying HANDLE. (os.NewFile registers
//     the handle with the IOCP-based poller.)
//
//   - The startup attribute list buffer MUST stay alive for the duration of
//     CreateProcess. We store it in the PtySession (as []byte) so the GC
//     keeps it pinned, then DeleteProcThreadAttributeList in CloseSession.
//
//   - On close, calling ClosePseudoConsole is what causes the child shell to
//     exit (it ends up with EOF on stdin and a broken stdout pipe). We still
//     fall back to TerminateProcess after a 5-second grace period in case
//     the child is misbehaving.

package toolbox

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows"
)

// Lazy DLL bindings — kernel32 is always loaded but the ConPTY procs only
// exist on Windows 10 1809+. .Find() returns an error which we surface
// (via CreatePseudoConsole's syscall.Errno fallback) when missing.
var (
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procCreatePseudoConsole          = modKernel32.NewProc("CreatePseudoConsole")
	procClosePseudoConsole           = modKernel32.NewProc("ClosePseudoConsole")
	procResizePseudoConsole          = modKernel32.NewProc("ResizePseudoConsole")
	procInitializeProcThreadAttrList = modKernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute    = modKernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttrList     = modKernel32.NewProc("DeleteProcThreadAttributeList")
)

const (
	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE — see Microsoft docs for ConPTY.
	procThreadAttributePseudoConsole = 0x00020016
	// CreateProcess flags.
	extendedStartupInfoPresent = 0x00080000
)

// startupInfoEx mirrors STARTUPINFOEXW. The attribute list pointer is kept
// as unsafe.Pointer (not uintptr) so the GC tracks it as a live reference
// to the underlying byte slice during the CreateProcess syscall.
type startupInfoEx struct {
	StartupInfo windows.StartupInfo
	AttrList    unsafe.Pointer
}

// packCoord packs a COORD {X, Y int16} into a uintptr suitable for passing
// by value to a Windows syscall. COORD is 4 bytes total; the calling
// convention places it in a single register slot.
func packCoord(cols, rows int) uintptr {
	return uintptr(uint32(uint16(cols)) | (uint32(uint16(rows)) << 16))
}

// PtySession holds state for an active Windows ConPTY session.
//
// The Pty field is retained as nil to keep the cross-platform PtySession
// struct shape identical to pty_unix.go (callers don't touch it but
// reflection-based tools might). Real I/O goes through inWrite / outRead.
type PtySession struct {
	ID           string
	Pty          *os.File // always nil on Windows; kept for API parity
	Width        int
	Height       int
	StartedAt    time.Time
	LastActivity time.Time
	closed       atomic.Bool

	// Windows internals — not exported.
	hpc         windows.Handle // PseudoConsole handle (HPCON)
	procHandle  windows.Handle // child process handle
	inWrite     *os.File       // our writable end of the keyboard pipe
	outRead     *os.File       // our readable end of the screen pipe
	attrListBuf []byte         // keep alive for GC during CreateProcess
	attrList    unsafe.Pointer // pointer into attrListBuf
}

// PtyManager tracks all active ConPTY sessions.
type PtyManager struct {
	mu       sync.RWMutex
	sessions map[string]*PtySession
}

// NewPtyManager creates a new PtyManager.
func NewPtyManager() *PtyManager {
	return &PtyManager{sessions: make(map[string]*PtySession)}
}

// CreateSession allocates a ConPTY and launches PowerShell attached to it.
func (m *PtyManager) CreateSession(width, height int) (*PtySession, error) {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}

	// 1. Build the two pipes. Each pipe has a "near" end (we keep) and a
	//    "far" end (ConPTY keeps). inRead is fed to ConPTY's keyboard
	//    input; outWrite is where ConPTY writes screen content.
	var inRead, inWrite windows.Handle
	var outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe(input): %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("CreatePipe(output): %w", err)
	}

	// 2. CreatePseudoConsole takes ownership of inRead+outWrite. After this
	//    call we close them locally; the only valid references live inside
	//    the ConPTY object.
	var hpc windows.Handle
	r1, _, callErr := procCreatePseudoConsole.Call(
		packCoord(width, height),
		uintptr(inRead),
		uintptr(outWrite),
		0, // dwFlags
		uintptr(unsafe.Pointer(&hpc)),
	)
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	// CreatePseudoConsole returns an HRESULT; non-zero is failure.
	if int32(r1) < 0 {
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("CreatePseudoConsole: HRESULT=0x%x (%v) — Windows 10 1809+ required", uint32(r1), callErr)
	}

	// 3. Set up the process-thread attribute list with one slot for the
	//    PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE entry. The first call probes
	//    the required buffer size (it intentionally fails with
	//    ERROR_INSUFFICIENT_BUFFER); the second initializes the list.
	var listSize uintptr
	procInitializeProcThreadAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&listSize)))
	if listSize == 0 {
		closePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("InitializeProcThreadAttributeList size probe returned 0")
	}
	attrListBuf := make([]byte, listSize)
	attrList := unsafe.Pointer(&attrListBuf[0])

	r1, _, callErr = procInitializeProcThreadAttrList.Call(
		uintptr(attrList),
		1,
		0,
		uintptr(unsafe.Pointer(&listSize)),
	)
	if r1 == 0 {
		closePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("InitializeProcThreadAttributeList: %v", callErr)
	}

	r1, _, callErr = procUpdateProcThreadAttribute.Call(
		uintptr(attrList),
		0, // dwFlags
		procThreadAttributePseudoConsole,
		uintptr(hpc),
		unsafe.Sizeof(hpc),
		0, // lpPreviousValue
		0, // lpReturnSize
	)
	if r1 == 0 {
		procDeleteProcThreadAttrList.Call(uintptr(attrList))
		closePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %v", callErr)
	}

	// 4. Build STARTUPINFOEXW with the attribute list and launch
	//    powershell.exe. We do NOT inherit handles (bInheritHandles=false)
	//    because ConPTY attaches stdio via the attribute, not inheritance.
	var siEx startupInfoEx
	siEx.StartupInfo.Cb = uint32(unsafe.Sizeof(siEx))
	siEx.AttrList = attrList

	cmdLine, err := windows.UTF16PtrFromString(`powershell.exe -NoProfile -NoLogo`)
	if err != nil {
		procDeleteProcThreadAttrList.Call(uintptr(attrList))
		closePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("UTF16PtrFromString: %w", err)
	}

	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		nil,    // appName
		cmdLine,
		nil,    // procAttrs
		nil,    // threadAttrs
		false,  // inheritHandles
		extendedStartupInfoPresent,
		nil,    // env (inherit parent)
		nil,    // currentDir (inherit parent)
		(*windows.StartupInfo)(unsafe.Pointer(&siEx)),
		&pi,
	)
	if err != nil {
		procDeleteProcThreadAttrList.Call(uintptr(attrList))
		closePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("CreateProcess powershell.exe: %w", err)
	}
	// We don't need the thread handle; only the process handle for later termination.
	windows.CloseHandle(pi.Thread)

	// 5. Wrap our kept pipe ends as *os.File so the Go runtime poller can
	//    drive blocking I/O without burning a thread.
	inWriteFile := os.NewFile(uintptr(inWrite), "pty-in")
	outReadFile := os.NewFile(uintptr(outRead), "pty-out")

	sessionID := fmt.Sprintf("pty-%d", time.Now().UnixNano())
	session := &PtySession{
		ID:           sessionID,
		Width:        width,
		Height:       height,
		StartedAt:    time.Now(),
		LastActivity: time.Now(),
		hpc:          hpc,
		procHandle:   pi.Process,
		inWrite:      inWriteFile,
		outRead:      outReadFile,
		attrListBuf:  attrListBuf,
		attrList:     attrList,
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	return session, nil
}

// GetSession returns the PTY session for the given ID.
func (m *PtyManager) GetSession(id string) (*PtySession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// CloseSession closes the ConPTY (which causes the child to exit) and
// releases all native resources. Idempotent — safe to call twice.
func (m *PtyManager) CloseSession(id string) error {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session not found")
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	if !session.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Closing the PseudoConsole causes the child to receive EOF on stdin
	// and a broken pipe on stdout, which is the normal way to make a
	// shell exit. Wait briefly for the child to terminate naturally;
	// kill it if it overstays the grace period.
	closePseudoConsole(session.hpc)

	if session.procHandle != 0 {
		ev, err := windows.WaitForSingleObject(session.procHandle, 5000)
		if err != nil || ev == uint32(windows.WAIT_TIMEOUT) {
			windows.TerminateProcess(session.procHandle, 1) //nolint:errcheck
			windows.WaitForSingleObject(session.procHandle, 1000) //nolint:errcheck
		}
		windows.CloseHandle(session.procHandle)
	}

	if session.attrList != nil {
		procDeleteProcThreadAttrList.Call(uintptr(session.attrList))
	}

	if session.inWrite != nil {
		session.inWrite.Close() //nolint:errcheck
	}
	if session.outRead != nil {
		session.outRead.Close() //nolint:errcheck
	}
	return nil
}

// ResizeSession updates the terminal dimensions for an existing ConPTY.
func (m *PtyManager) ResizeSession(id string, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("invalid dimensions: %dx%d", width, height)
	}
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found")
	}

	r1, _, callErr := procResizePseudoConsole.Call(
		uintptr(session.hpc),
		packCoord(width, height),
	)
	if int32(r1) < 0 {
		return fmt.Errorf("ResizePseudoConsole: HRESULT=0x%x (%v)", uint32(r1), callErr)
	}
	m.mu.Lock()
	session.Width = width
	session.Height = height
	m.mu.Unlock()
	return nil
}

// ListSessions returns all active PTY sessions.
func (m *PtyManager) ListSessions() []*PtySession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*PtySession, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// closePseudoConsole is a thin wrapper because ClosePseudoConsole has a
// void return; we discard the syscall.Errno because there's no meaningful
// failure case (it just frees the kernel object).
func closePseudoConsole(hpc windows.Handle) {
	procClosePseudoConsole.Call(uintptr(hpc))
}

// PtyHandler bridges ConPTY sessions to WebSocket connections.
type PtyHandler struct {
	manager *PtyManager
}

// NewPtyHandler creates a new PtyHandler with its own PtyManager.
func NewPtyHandler() *PtyHandler {
	return &PtyHandler{manager: NewPtyManager()}
}

// CreateSession creates a new ConPTY session and returns its ID.
func (h *PtyHandler) CreateSession(width, height int) (string, error) {
	session, err := h.manager.CreateSession(width, height)
	if err != nil {
		return "", err
	}
	return session.ID, nil
}

// HandleWebSocket proxies bytes between the WebSocket connection and the
// ConPTY in both directions until either side closes. The protocol matches
// pty_unix.go: binary frames are raw terminal bytes, and the IAC NAWS
// (Telnet "negotiate about window size") escape sequence triggers a resize.
func (h *PtyHandler) HandleWebSocket(conn *websocket.Conn, sessionID string) error {
	session, ok := h.manager.GetSession(sessionID)
	if !ok {
		return fmt.Errorf("session not found")
	}

	session.LastActivity = time.Now()

	done := make(chan struct{})
	closeOnce := sync.Once{}

	// PTY → WebSocket. We read from the ConPTY output pipe; when ConPTY
	// terminates (child exited / session closed), the pipe returns EOF
	// and this goroutine signals shutdown.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := session.outRead.Read(buf)
			if n > 0 {
				session.LastActivity = time.Now()
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					fmt.Printf("WebSocket write error: %v\n", writeErr)
					closeOnce.Do(func() { close(done) })
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					fmt.Printf("ConPTY read error: %v\n", err)
				}
				closeOnce.Do(func() { close(done) })
				return
			}
		}
	}()

	// WebSocket → PTY.
	go func() {
		for {
			msgType, msg, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					fmt.Printf("WebSocket read error: %v\n", err)
				}
				closeOnce.Do(func() { close(done) })
				return
			}

			if msgType == websocket.CloseMessage {
				closeOnce.Do(func() { close(done) })
				return
			}

			session.LastActivity = time.Now()

			// IAC SB NAWS <Cols-Hi> <Cols-Lo> <Rows-Hi> <Rows-Lo> IAC SE.
			// Same encoding as the Unix handler so xterm.js clients work
			// unchanged across platforms.
			if len(msg) >= 6 && msg[0] == 0xFF && msg[1] == 0xFD && msg[2] == 0x1C {
				if len(msg) >= 8 && msg[5] == 0xFF && msg[6] == 0xFF {
					width := int(msg[3])<<8 | int(msg[4])
					height := int(msg[5])<<8 | int(msg[6])
					h.manager.ResizeSession(sessionID, width, height) //nolint:errcheck
					continue
				}
			}

			if _, err := session.inWrite.Write(msg); err != nil {
				fmt.Printf("ConPTY write error: %v\n", err)
				closeOnce.Do(func() { close(done) })
				return
			}
		}
	}()

	<-done
	h.manager.CloseSession(sessionID) //nolint:errcheck
	return nil
}

// CloseSession closes the PTY session with the given ID.
func (h *PtyHandler) CloseSession(sessionID string) error {
	return h.manager.CloseSession(sessionID)
}
