//go:build windows

package engine

import (
	"syscall"
	"unsafe"
)

// modkernel32 and procOpenProcess are already declared in jobobject_windows.go
// (this file is compiled into the same package, same build tag) — reused
// here rather than redeclared.
var (
	modpsapi = syscall.NewLazyDLL("psapi.dll")
	// GetProcessMemoryInfo (not the K32-prefixed name — that one's a
	// kernel32.dll forwarder, not a real psapi.dll export) is what
	// psapi.dll actually exports this under.
	procGetProcessMemoryInfo = modpsapi.NewProc("GetProcessMemoryInfo")
)

const (
	processQueryInformation = 0x0400
	processVMRead           = 0x0010
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS from psapi.h — only
// CB (struct size, required by the API) and WorkingSetSize are used.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// processRSSBytes reports pid's current working set size (bytes resident in
// RAM) via OpenProcess + GetProcessMemoryInfo — used as a rough progress
// proxy while a model is loading (see text.go's loadProgress heartbeat),
// since llama.cpp memory-maps the weight file and its working set grows
// roughly in step with how much of it has been read so far.
//
// This is a "best info we can get, never worth failing over" helper — a
// LazyProc.Call panics outright if the DLL/procedure can't be resolved
// (confirmed the hard way: a wrong procedure name here once took down the
// entire in-flight chat generation goroutine it was called from, silently,
// leaving the user with no reply and no error — the progress heartbeat is
// not something a caller several layers away should ever be able to be
// killed by). The recover here is the actual fix for that class of bug,
// not just the corrected procedure name — any future syscall-layer surprise
// on some Windows version/build degrades to "no percentage available"
// instead of ever propagating up again.
func processRSSBytes(pid int) (rss uint64, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			rss, ok = 0, false
		}
	}()
	h, _, _ := procOpenProcess.Call(uintptr(processQueryInformation|processVMRead), 0, uintptr(pid))
	if h == 0 {
		return 0, false
	}
	defer syscall.CloseHandle(syscall.Handle(h))

	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	ret, _, _ := procGetProcessMemoryInfo.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	if ret == 0 {
		return 0, false
	}
	return uint64(pmc.WorkingSetSize), true
}
