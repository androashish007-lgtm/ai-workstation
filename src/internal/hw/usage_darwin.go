//go:build darwin

package hw

import (
	"os/exec"
	"regexp"
	"strconv"
)

// macOS has no /proc/stat equivalent reachable without cgo/mach calls, so
// we shell out to `top`, which already does its own two-sample delta
// internally (`-l 2`) and reports a ready-to-use idle percentage.
var cpuUsageRe = regexp.MustCompile(`CPU usage:\s*[\d.]+%\s*user,\s*[\d.]+%\s*sys,\s*([\d.]+)%\s*idle`)

func sampleCPUPercent() (float64, bool) {
	out, err := exec.Command("top", "-l", "2", "-n", "0", "-stats", "cpu").Output()
	if err != nil {
		return 0, false
	}
	matches := cpuUsageRe.FindAllStringSubmatch(string(out), -1)
	if len(matches) == 0 {
		return 0, false
	}
	last := matches[len(matches)-1] // second sample is the delta-based, accurate one
	idlePct, err := strconv.ParseFloat(last[1], 64)
	if err != nil {
		return 0, false
	}
	busy := 100 - idlePct
	if busy < 0 {
		busy = 0
	}
	if busy > 100 {
		busy = 100
	}
	return busy, true
}
