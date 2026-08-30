//go:build linux

// Copyright 2026 SandrPod Contributors
// cgroup limits for the sandbox's own resource metrics.
//
// /proc/meminfo, /proc/stat and runtime.NumCPU() are not namespaced: inside a
// container they describe the HOST. A sandbox capped at 512 MiB and one core
// reported "4 cores / 7.44 GiB" — the machine it happened to land on. The
// cgroup is the only place the container's real limits appear, so read it
// first and fall back to the host view when there is no limit (or no cgroup,
// as on a bare-metal sandrpod-agent, where the host view is the right answer).

package toolbox

import (
	"os"
	"strconv"
	"strings"
)

// cgroupRoot is where the container's own cgroup is mounted. A variable so
// tests can point it at a fixture directory.
var cgroupRoot = "/sys/fs/cgroup"

// readCgroupUint reads a single-value cgroup file. ok is false when the file is
// absent, unparseable, or holds v2's "max" (meaning: no limit).
func readCgroupUint(rel string) (uint64, bool) {
	b, err := os.ReadFile(cgroupRoot + "/" + rel)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// unlimitedV1 is what cgroup v1 writes for "no limit" — near the top of int64,
// varying by page size across kernels, so compare by magnitude rather than
// against one exact constant.
const unlimitedV1 = uint64(1) << 62

// cgroupMemory returns the container's memory limit and current usage.
// v2 (memory.max / memory.current) is tried first, then v1.
func cgroupMemory() (limit, used uint64, ok bool) {
	if l, got := readCgroupUint("memory.max"); got && l < unlimitedV1 {
		u, _ := readCgroupUint("memory.current")
		return l, u, true
	}
	if l, got := readCgroupUint("memory/memory.limit_in_bytes"); got && l < unlimitedV1 {
		u, _ := readCgroupUint("memory/memory.usage_in_bytes")
		return l, u, true
	}
	return 0, 0, false
}

// cgroupCPUQuota returns the container's CPU allowance in cores (1.5 means one
// and a half). v2 stores "<quota> <period>" in cpu.max; v1 splits the two.
func cgroupCPUQuota() (float64, bool) {
	if b, err := os.ReadFile(cgroupRoot + "/cpu.max"); err == nil {
		f := strings.Fields(strings.TrimSpace(string(b)))
		if len(f) == 2 && f[0] != "max" {
			q, err1 := strconv.ParseFloat(f[0], 64)
			p, err2 := strconv.ParseFloat(f[1], 64)
			if err1 == nil && err2 == nil && p > 0 && q > 0 {
				return q / p, true
			}
		}
	}
	q, ok1 := readCgroupUint("cpu/cpu.cfs_quota_us")
	p, ok2 := readCgroupUint("cpu/cpu.cfs_period_us")
	if ok1 && ok2 && p > 0 && q > 0 && q < unlimitedV1 {
		return float64(q) / float64(p), true
	}
	return 0, false
}

// cgroupCPUUsec returns cumulative CPU time used by this cgroup, microseconds.
func cgroupCPUUsec() (uint64, bool) {
	if b, err := os.ReadFile(cgroupRoot + "/cpu.stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, found := strings.CutPrefix(line, "usage_usec "); found {
				if v, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64); err == nil {
					return v, true
				}
			}
		}
	}
	// v1 reports nanoseconds.
	if v, ok := readCgroupUint("cpuacct/cpuacct.usage"); ok {
		return v / 1000, true
	}
	return 0, false
}
