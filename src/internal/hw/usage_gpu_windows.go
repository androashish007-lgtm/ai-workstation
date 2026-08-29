//go:build windows

package hw

import (
	"os/exec"
	"strconv"
	"strings"
)

// sampleGPUPercent reports aggregate GPU engine utilization via the same
// "GPU Engine" performance counter set Task Manager's GPU tab reads from —
// summed across every engine instance (3D, Compute, Copy, etc.) and clamped
// to 100, since a GPU with multiple active engines can otherwise sum past
// it. This is noticeably slower than the CPU sampler (roughly 1-1.5s per
// call, inherent to this counter set, not something callable more often
// helps with) — callers should poll this far less frequently than CPU/RAM.
func sampleGPUPercent() (float64, bool) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-Counter '\\GPU Engine(*)\\Utilization Percentage' -ErrorAction SilentlyContinue).CounterSamples "+
			"| Measure-Object -Property CookedValue -Sum "+
			"| Select-Object -ExpandProperty Sum").Output()
	if err != nil {
		return 0, false
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, false
	}
	if val < 0 {
		val = 0
	}
	if val > 100 {
		val = 100
	}
	return val, true
}
