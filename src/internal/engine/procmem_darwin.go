//go:build darwin

package engine

import (
	"os/exec"
	"strconv"
	"strings"
)

// processRSSBytes shells out to `ps` for pid's resident set size (in KB) —
// see procmem_windows.go's doc comment for why this is used (a progress
// proxy while a model is loading). macOS has no simple /proc equivalent
// and no cgo-free syscall for this, but `ps` is always present.
func processRSSBytes(pid int) (uint64, bool) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return kb * 1024, true
}
