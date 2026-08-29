//go:build linux

package hw

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
)

var (
	cpuStateMu          sync.Mutex
	prevIdle, prevTotal uint64
	havePrevCPU         bool
)

// sampleCPUPercent reports overall CPU utilization since the previous call,
// by diffing the aggregate "cpu " line of /proc/stat (jiffy counters since
// boot, in the order: user, nice, system, idle, iowait, irq, softirq,
// steal, guest, guest_nice).
func sampleCPUPercent() (float64, bool) {
	idle, total, ok := readProcStatCPU()
	if !ok {
		return 0, false
	}

	cpuStateMu.Lock()
	defer cpuStateMu.Unlock()
	if !havePrevCPU {
		prevIdle, prevTotal = idle, total
		havePrevCPU = true
		return 0, false
	}
	idleDelta := diffU64(idle, prevIdle)
	totalDelta := diffU64(total, prevTotal)
	prevIdle, prevTotal = idle, total
	if totalDelta == 0 {
		return 0, false
	}
	busy := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	if busy < 0 {
		busy = 0
	}
	if busy > 100 {
		busy = 100
	}
	return busy, true
}

func readProcStatCPU() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var vals []uint64
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break
		}
		vals = append(vals, v)
		total += v
	}
	if len(vals) < 4 {
		return 0, 0, false
	}
	idle = vals[3] // idle
	if len(vals) >= 5 {
		idle += vals[4] // + iowait, conventionally counted as idle too
	}
	return idle, total, true
}

func diffU64(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
