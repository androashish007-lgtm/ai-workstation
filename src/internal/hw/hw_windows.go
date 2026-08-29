//go:build windows

package hw

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX struct. We call
// GlobalMemoryStatusEx directly via syscall (stdlib, no extra dependency)
// rather than pulling in golang.org/x/sys/windows just for this one call.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var (
	modkernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

func readWindowsMem() (total, avail uint64) {
	var status memoryStatusEx
	status.Length = uint32(unsafe.Sizeof(status))
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 {
		return 0, 0
	}
	return status.TotalPhys, status.AvailPhys
}
