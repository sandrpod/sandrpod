//go:build linux

package toolbox

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture points cgroupRoot at a temp dir holding the given files.
func fixture(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev := cgroupRoot
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = prev })
}

// A sandbox created with --memory 512 --cpu 1 reported "4 cores / 7.44 GiB" —
// the host it landed on. /proc/meminfo and NumCPU are not namespaced; only the
// cgroup knows the container's own limits.
func TestCgroupMemory_V2(t *testing.T) {
	fixture(t, map[string]string{
		"memory.max":     "536870912\n", // exactly 512 MiB, as Docker writes it
		"memory.current": "104857600\n",
	})
	limit, used, ok := cgroupMemory()
	if !ok || limit != 536870912 || used != 104857600 {
		t.Fatalf("got (%d, %d, %v), want (536870912, 104857600, true)", limit, used, ok)
	}
}

func TestCgroupMemory_V1(t *testing.T) {
	fixture(t, map[string]string{
		"memory/memory.limit_in_bytes": "268435456\n",
		"memory/memory.usage_in_bytes": "1048576\n",
	})
	limit, _, ok := cgroupMemory()
	if !ok || limit != 268435456 {
		t.Fatalf("v1 limit = %d (ok=%v), want 268435456", limit, ok)
	}
}

// No limit must fall back to the host view rather than reporting a bogus one.
func TestCgroupMemory_Unlimited(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"v2 max":      {"memory.max": "max\n"},
		"v1 sentinel": {"memory/memory.limit_in_bytes": "9223372036854771712\n"},
		"no cgroup":   {},
		"unparseable": {"memory.max": "not-a-number\n"},
	} {
		fixture(t, files)
		if _, _, ok := cgroupMemory(); ok {
			t.Errorf("%s: reported a limit where there is none", name)
		}
	}
}

func TestCgroupCPUQuota(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  float64
		ok    bool
	}{
		{"v2 one core", map[string]string{"cpu.max": "100000 100000\n"}, 1, true},
		{"v2 half core", map[string]string{"cpu.max": "50000 100000\n"}, 0.5, true},
		{"v2 unlimited", map[string]string{"cpu.max": "max 100000\n"}, 0, false},
		{"v1 two cores", map[string]string{
			"cpu/cpu.cfs_quota_us":  "200000\n",
			"cpu/cpu.cfs_period_us": "100000\n",
		}, 2, true},
		{"v1 unlimited", map[string]string{
			"cpu/cpu.cfs_quota_us":  "-1\n",
			"cpu/cpu.cfs_period_us": "100000\n",
		}, 0, false},
		{"absent", map[string]string{}, 0, false},
	}
	for _, tc := range cases {
		fixture(t, tc.files)
		got, ok := cgroupCPUQuota()
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCgroupCPUUsec(t *testing.T) {
	fixture(t, map[string]string{
		"cpu.stat": "usage_usec 123456\nuser_usec 100000\nsystem_usec 23456\n",
	})
	if v, ok := cgroupCPUUsec(); !ok || v != 123456 {
		t.Fatalf("v2 usage = %d (ok=%v), want 123456", v, ok)
	}
	fixture(t, map[string]string{"cpuacct/cpuacct.usage": "5000000\n"}) // ns
	if v, ok := cgroupCPUUsec(); !ok || v != 5000 {
		t.Fatalf("v1 usage = %d (ok=%v), want 5000 (ns→µs)", v, ok)
	}
}

// The reported CPUCount must reflect the allowance, rounding a fractional one
// up so half a core reads as 1 rather than 0.
func TestCollectPlatformMetrics_UsesCgroupLimits(t *testing.T) {
	fixture(t, map[string]string{
		"memory.max":     "536870912\n",
		"memory.current": "52428800\n",
		"cpu.max":        "50000 100000\n", // half a core
		"cpu.stat":       "usage_usec 0\n",
	})
	m := Metrics{CPUCount: 64} // pretend a very large host
	collectPlatformMetrics(&m)
	if m.MemTotal != 536870912 {
		t.Errorf("MemTotal = %d, want the cgroup's 536870912", m.MemTotal)
	}
	if m.MemUsed != 52428800 {
		t.Errorf("MemUsed = %d, want the cgroup's 52428800", m.MemUsed)
	}
	if m.CPUCount != 1 {
		t.Errorf("CPUCount = %d, want 1 for a half-core allowance on a 64-core host", m.CPUCount)
	}
}
