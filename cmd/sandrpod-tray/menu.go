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
	"github.com/sandrpod/sandrpod/pkg/brand"
)

// trayIcon (shield + check) is embedded per-platform — see icon_unix.go
// (PNG for macOS/Linux) and icon_windows.go (ICO: the Windows tray loads
// icons via LoadImage(IMAGE_ICON), which rejects PNG bytes and renders an
// empty icon). Regenerate both assets with scripts/gen_tray_icon.py.

func onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle(brand.Name()) // shown next to the icon on macOS; no-op on Windows
	systray.SetTooltip(brand.Name() + " Sandbox 权限守护")

	// Header (disabled, just a label)
	header := systray.AddMenuItem(brand.Name()+" Sandbox", "")
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
