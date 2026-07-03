//go:build !linux

// Copyright 2024 SandrPod
// Non-Linux metrics fallback. The toolbox targets Linux sandbox containers in
// production; on other platforms (a macOS/Windows agent) only the CPU count is
// reported and the rest is left zero.

package toolbox

func collectPlatformMetrics(m *Metrics) {}
