// Copyright 2026 SandrPod
//go:build windows

package main

import _ "embed"

// trayIcon is the system-tray image. systray's Windows backend writes the
// bytes to a temp file and loads them with LoadImage(IMAGE_ICON), which
// only accepts .ico — PNG bytes render as an empty icon. The .ico carries
// 16/24/32/48 px BMP-format entries for LoadImage compatibility across
// DPI scales.
//
//go:embed assets/icon.ico
var trayIcon []byte
