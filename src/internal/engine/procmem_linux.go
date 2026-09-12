//go:build linux

package engine

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processRSSBytes reads pid's resident set size from /proc/<pid>/status —
// see procmem_windows.go's doc comment for why this is used (a progress
// proxy while a model is loading). Covers Termux/Android too (same GOOS).
func processRSSBytes(pid int) (uint64, bool) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}
