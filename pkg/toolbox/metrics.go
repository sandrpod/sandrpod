// Copyright 2024 SandrPod
// Sandbox resource metrics backing the E2B get_metrics surface. The toolbox
// reports its own CPU / memory / disk usage; the server wraps a single reading
// in the E2B SandboxMetric shape.

package toolbox

import "runtime"

// Metrics is a point-in-time resource snapshot of the sandbox.
type Metrics struct {
	CPUCount   int     `json:"cpu_count"`
	CPUUsedPct float64 `json:"cpu_used_pct"`
	MemTotal   uint64  `json:"mem_total"` // bytes
	MemUsed    uint64  `json:"mem_used"`  // bytes
	DiskTotal  uint64  `json:"disk_total"`
	DiskUsed   uint64  `json:"disk_used"`
}

// CollectMetrics gathers a resource snapshot. CPU percent is sampled over a
// short interval (see the platform implementation); the count always reflects
// the visible CPUs.
func CollectMetrics() Metrics {
	m := Metrics{CPUCount: runtime.NumCPU()}
	collectPlatformMetrics(&m)
	return m
}
