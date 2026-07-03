//go:build linux

// Copyright 2024 SandrPod
// Linux resource metrics: /proc/meminfo for memory, /proc/stat sampled twice
// for CPU percent, statfs for disk. This is the production path (the toolbox
// runs in Linux sandbox containers).

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
	m.MemTotal, m.MemUsed = readMemInfo()
	m.CPUUsedPct = sampleCPUPercent()
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

// sampleCPUPercent reads /proc/stat twice ~100ms apart and returns the busy
// fraction of total jiffies as a percentage.
func sampleCPUPercent() float64 {
	idle1, total1 := readCPUJiffies()
	time.Sleep(100 * time.Millisecond)
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
