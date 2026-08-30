//go:build linux

// Copyright 2026 SandrPod Contributors
// Linux resource metrics. The cgroup is consulted first — /proc/meminfo,
// /proc/stat and NumCPU describe the HOST inside a container, so a capped
// sandbox otherwise reports the machine it landed on. The host view remains
// the fallback, and the right answer where there is no limit (or no cgroup,
// as under a bare-metal sandrpod-agent). Disk stays statfs: nothing sets a
// per-container quota, so the filesystem really is shared.

package toolbox

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func collectPlatformMetrics(m *Metrics) {
	if limit, used, ok := cgroupMemory(); ok {
		m.MemTotal, m.MemUsed = limit, used
	} else {
		m.MemTotal, m.MemUsed = readMemInfo()
	}

	cores := float64(m.CPUCount)
	if q, ok := cgroupCPUQuota(); ok {
		cores = q
		// CPUCount is an integer; round a fractional allowance up so half a
		// core reads as 1 rather than 0.
		m.CPUCount = int(q)
		if float64(m.CPUCount) < q {
			m.CPUCount++
		}
	}
	m.CPUUsedPct = sampleCPUPercent(cores)
	m.DiskTotal, m.DiskUsed = readDisk(defaultWorkDir())
}

// readMemInfo parses /proc/meminfo. Used = MemTotal - MemAvailable.
func readMemInfo() (total, used uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var memTotal, memAvail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		v *= 1024 // kB → bytes
		switch fields[0] {
		case "MemTotal:":
			memTotal = v
		case "MemAvailable:":
			memAvail = v
		}
	}
	if memAvail > memTotal {
		memAvail = memTotal
	}
	return memTotal, memTotal - memAvail
}

// sampleCPUPercent returns busy CPU as a percentage of `cores`. It samples the
// cgroup's own consumed CPU time when available — /proc/stat is host-wide, so
// on a busy host a completely idle sandbox reported the host's load as its own.
func sampleCPUPercent(cores float64) float64 {
	if u1, ok := cgroupCPUUsec(); ok && cores > 0 {
		time.Sleep(cpuSampleInterval)
		if u2, ok2 := cgroupCPUUsec(); ok2 && u2 >= u1 {
			busy := float64(u2-u1) / float64(cpuSampleInterval.Microseconds())
			pct := busy / cores * 100
			if pct > 100 {
				pct = 100
			}
			return pct
		}
	}
	return sampleHostCPUPercent()
}

// cpuSampleInterval is how long the two CPU readings are spaced. Long enough to
// be meaningful, short enough that a metrics call stays snappy.
const cpuSampleInterval = 100 * time.Millisecond

// sampleHostCPUPercent reads /proc/stat twice and returns the busy fraction of
// total jiffies as a percentage. Host-wide; used when there is no cgroup.
func sampleHostCPUPercent() float64 {
	idle1, total1 := readCPUJiffies()
	time.Sleep(cpuSampleInterval)
	idle2, total2 := readCPUJiffies()
	dt := total2 - total1
	if dt == 0 {
		return 0
	}
	di := idle2 - idle1
	return (1 - float64(di)/float64(dt)) * 100
}

func readCPUJiffies() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		for i, fld := range fields {
			v, _ := strconv.ParseUint(fld, 10, 64)
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		break
	}
	return idle, total
}

// readDisk returns the total and used bytes of the filesystem holding path.
func readDisk(path string) (total, used uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	used = (st.Blocks - st.Bfree) * bsize
	return total, used
}
