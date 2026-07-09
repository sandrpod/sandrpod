// Copyright 2026 SandrPod
//go:build !windows

package main

import _ "embed"

// trayIcon is the menu-bar / system-tray image. macOS and Linux status
// trays accept PNG bytes directly.
//
//go:embed assets/icon.png
var trayIcon []byte
