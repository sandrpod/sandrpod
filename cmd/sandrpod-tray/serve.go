// Copyright 2026 SandrPod
// `sandrpod-tray serve` — the always-on user-session daemon.
//
// Lifecycle:
//
//   1. Load (or bootstrap) ~/.sandrpod/permissions.json. On first run install
//      the hardlock seeds so the employee never sits unprotected.
//   2. Open the IPC socket so sandrpod-agent can ask consent.
//   3. Start a tiny HTTP server on 127.0.0.1:<random> for the settings page.
//   4. Hand the main goroutine to systray.Run; the menu wires up to all of
//      the above (open settings page, reload, pause, quit).
//
// systray.Run blocks the calling goroutine on macOS and Windows because the
// underlying GUI APIs require it. Everything else (IPC, HTTP) runs in
// background goroutines.

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/getlantern/systray"
	"github.com/sandrpod/sandrpod/pkg/notify"
	"github.com/sandrpod/sandrpod/pkg/permission"
)

// runState is the package-level handle the tray menu uses to reach into the
// running services. Globals are normally a smell, but systray's
// onReady/onExit callback API offers no place to thread a struct through, so
// we keep them here and make them thread-safe via atomic.Value where mutated.
var (
	runStore       *permission.Store
	runIPC         *permission.IPCServer
	runHTTPAddr    atomic.Value // string, set after HTTP server binds
	runPausedUntil atomic.Value // time.Time, zero when not paused

	// activeNotifier is replaced when the user pauses prompts so any
	// in-flight Ask() coming from the IPC server still sees a coherent state.
	activeNotifier atomic.Value // permission.Notifier
)

// runServe is the `serve` subcommand entry point.
func runServe() {
	store, err := permission.LoadStore(storePath())
	if err != nil {
		log.Fatalf("load permissions.json: %v", err)
	}

	// First-run protection: if the store is empty, plant the default
	// hardlocks before we start answering IPC requests. This guarantees
	// fresh installs are protected even if the employee never opens the
	// settings page.
	if added, err := permission.SeedHardlocksIfEmpty(store); err == nil && added > 0 {
		log.Printf("first run: installed %d default hardlock seeds", added)
	}
	if seeded, err := permission.SeedCommandPolicyIfEmpty(store); err == nil && seeded {
		log.Printf("first run: installed default command policy (deny + warn lists)")
	}
	runStore = store

	// Tray runs in the user session so it has GUI access on every supported
	// OS. NewPrompter selects osascript on macOS, zenity/kdialog on Linux,
	// PowerShell MessageBox on Windows.
	prompter := notify.NewPrompter()
	activeNotifier.Store(prompter)

	// IPC server delegates to a tiny adapter that reads activeNotifier each
	// time so "暂停 1 小时" can swap in a deny-everything notifier without
	// tearing down the listener.
	ipc := permission.NewIPCServer(socketPath(), notifierProxy{})
	if err := ipc.Start(context.Background()); err != nil {
		log.Fatalf("start IPC server on %s: %v", socketPath(), err)
	}
	runIPC = ipc
	log.Printf("IPC server listening at %s", socketPath())

	if err := startHTTP(); err != nil {
		log.Printf("settings HTTP server failed to start: %v (tray will still work, but the settings page is unavailable)", err)
	}

	systray.Run(onTrayReady, onTrayExit)
}

// notifierProxy delegates Ask() to whichever Notifier is currently installed
// in the global atomic. Used so the menu can swap "paused" / "active"
// notifiers without rebinding the socket.
type notifierProxy struct{}

func (notifierProxy) Ask(ctx context.Context, req permission.Request) (permission.PromptResponse, error) {
	// Honor pause: if the user clicked "暂停 1 小时", auto-deny without
	// disturbing them. We log so the agent-side audit can show "denied while
	// paused" rather than "denied by user".
	if pausedUntil, ok := runPausedUntil.Load().(time.Time); ok && !pausedUntil.IsZero() {
		if time.Now().Before(pausedUntil) {
			log.Printf("auto-denied while paused: %s mode=%s", req.Path, req.Mode)
			return permission.PromptDeny, nil
		}
		// Pause expired — clear it so future loads don't see stale value.
		runPausedUntil.Store(time.Time{})
	}

	n, _ := activeNotifier.Load().(permission.Notifier)
	if n == nil {
		return permission.PromptDeny, nil
	}
	return n.Ask(ctx, req)
}

// startHTTP brings up the settings server on 127.0.0.1:<random>. We reach
// the chosen port out via runHTTPAddr so the tray "授权管理…" menu can pop
// the right URL. Binding to 127.0.0.1 (not 0.0.0.0) means the page is
// reachable only from this machine — the tray serves no auth itself, so
// loopback is the security boundary.
func startHTTP() error {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpIndex)
	mux.HandleFunc("/api/snapshot", httpSnapshot)
	mux.HandleFunc("/api/rules/add", httpRuleAdd)
	mux.HandleFunc("/api/rules/rm", httpRuleRemove)
	mux.HandleFunc("/api/policy/upsert", httpPolicyUpsert)
	mux.HandleFunc("/api/policy/rm", httpPolicyRemove)
	mux.HandleFunc("/api/mcp/manifest", httpMCPManifest)
	mux.HandleFunc("/api/mcp/reload", httpMCPReload)
	mux.HandleFunc("/api/mcp/server", httpMCPServerAction)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server: %v", err)
		}
	}()
	addr := fmt.Sprintf("http://%s", lis.Addr().String())
	runHTTPAddr.Store(addr)
	log.Printf("settings page available at %s", addr)
	return nil
}

// openInBrowser shells out to the platform-native "open this URL/path"
// helper. We skip error handling because there's no good UX for "your
// machine has no default browser" — at worst the menu click is a no-op.
func openInBrowser(target string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", target).Start()
	case "linux":
		_ = exec.Command("xdg-open", target).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	}
}
