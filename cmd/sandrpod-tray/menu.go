// Copyright 2026 SandrPod
// systray menu wiring for `sandrpod-tray serve`.
//
// systray is callback-based: onTrayReady is invoked once when the icon is
// shown; we register menu items and goroutines that listen for clicks. There
// is no "redraw" — we mutate item titles in-place to reflect state changes
// (e.g. "暂停 1 小时" ↔ "恢复").

package main

import (
	"log"
	"time"

	"github.com/getlantern/systray"
)

// trayIcon is the embedded PNG bytes shown in the menu bar / system tray.
// macOS will tint a monochrome image automatically; on Windows/Linux we
// ship a simple opaque PNG. Sprint 2 uses a placeholder; Sprint 4 will
// replace with a proper Acme asset.
//
// 1×1 transparent PNG — minimal valid bytes so systray has *something*
// without us shipping a real asset yet.
var trayIcon = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("🛡 Acme") // text fallback for environments where icons render poorly
	systray.SetTooltip("Acme Sandbox 权限守护")

	// Header (disabled, just a label)
	header := systray.AddMenuItem("Acme Sandbox", "")
	header.Disable()

	statusItem := systray.AddMenuItem(statusLabel(), "IPC server status")
	statusItem.Disable()
	go refreshStatusLoop(statusItem)

	systray.AddSeparator()

	openSettings := systray.AddMenuItem("授权管理…", "在浏览器中打开设置页")
	openJSON := systray.AddMenuItem("查看 permissions.json", "用默认编辑器打开规则文件")
	systray.AddSeparator()

	pause := systray.AddMenuItem("暂停 1 小时", "暂停期间所有未授权请求自动拒绝")
	resume := systray.AddMenuItem("恢复", "立即结束暂停状态")
	resume.Hide()
	systray.AddSeparator()

	// MCP transport bridge submenu — talks to the agent over a local
	// unix socket. Gracefully shows "未连接" when the agent isn't running.
	initMCPMenu()
	systray.AddSeparator()

	about := systray.AddMenuItem("关于 sandrpod-tray", "")
	quit := systray.AddMenuItem("退出（停止 IPC 服务）", "")

	go func() {
		for {
			select {
			case <-openSettings.ClickedCh:
				if addr, ok := runHTTPAddr.Load().(string); ok && addr != "" {
					openInBrowser(addr)
				} else {
					log.Println("settings HTTP server not yet ready; retry in a moment")
				}

			case <-openJSON.ClickedCh:
				openInBrowser(storePath())

			case <-pause.ClickedCh:
				until := time.Now().Add(time.Hour)
				runPausedUntil.Store(until)
				pause.Hide()
				resume.Show()
				log.Printf("prompts paused until %s", until.Format(time.Kitchen))

			case <-resume.ClickedCh:
				runPausedUntil.Store(time.Time{})
				resume.Hide()
				pause.Show()
				log.Println("prompts resumed")

			case <-about.ClickedCh:
				openInBrowser("https://github.com/sandrpod/sandrpod")

			case <-quit.ClickedCh:
				log.Println("quit requested via tray menu")
				systray.Quit()
				return
			}
		}
	}()
}

func onTrayExit() {
	if runIPC != nil {
		runIPC.Stop()
	}
	log.Println("sandrpod-tray exiting")
}

// statusLabel returns the current status string used both at startup and on
// each menu refresh. Kept as a function so we can extend later (e.g. show
// "暂停中 (剩 12 分钟)").
func statusLabel() string {
	if pausedUntil, ok := runPausedUntil.Load().(time.Time); ok && !pausedUntil.IsZero() && time.Now().Before(pausedUntil) {
		mins := int(time.Until(pausedUntil).Minutes())
		return "○ 暂停中（剩 " + itoa(mins) + " 分钟）"
	}
	return "● 在线"
}

// refreshStatusLoop ticks the status row so paused-countdown stays current
// without forcing the user to close/reopen the menu.
func refreshStatusLoop(item *systray.MenuItem) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		item.SetTitle(statusLabel())
	}
}

// itoa is a stripped-down strconv.Itoa to avoid importing strconv just for
// the status string. Kept inline because changing the call site to use
// strconv is the more honest fix if we ever need padding/format options.
func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
