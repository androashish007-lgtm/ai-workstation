//go:build windows

package hw

import (
	"sync"
	"unsafe"
)

var (
	procGetSystemTimes = modkernel32.NewProc("GetSystemTimes")

	cpuStateMu                     sync.Mutex
	prevIdle, prevKernel, prevUser uint64
	havePrevCPU                    bool
)

type fileTime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func fileTimeToUint64(ft fileTime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// sampleCPUPercent reports overall CPU utilization since the previous call,
// using GetSystemTimes (idle/kernel/user 100ns counters since boot — kernel
// time includes idle time, per the Win32 API contract).
func sampleCPUPercent() (float64, bool) {
	var idle, kernel, user fileTime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return 0, false
	}
	idleV, kernelV, userV := fileTimeToUint64(idle), fileTimeToUint64(kernel), fileTimeToUint64(user)

	cpuStateMu.Lock()
	defer cpuStateMu.Unlock()
	if !havePrevCPU {
		prevIdle, prevKernel, prevUser = idleV, kernelV, userV
		havePrevCPU = true
		return 0, false
	}
	idleDelta := diffU64(idleV, prevIdle)
	totalDelta := diffU64(kernelV, prevKernel) + diffU64(userV, prevUser)
	prevIdle, prevKernel, prevUser = idleV, kernelV, userV
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

func diffU64(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
