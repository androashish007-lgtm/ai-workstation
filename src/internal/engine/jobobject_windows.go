//go:build windows

package engine

import (
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// On Windows, this app's own process dying — cleanly, crashed, or force-
// killed via Task Manager/taskkill — does NOT automatically kill the
// llama-server/sd-cli child processes it started. They keep running as
// orphans, quietly holding onto however much RAM their model had loaded,
// until something notices and kills them by hand. A Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE fixes this at the OS level: every
// child assigned to it dies the instant this process does, for any
// reason — not just the graceful-shutdown path this app's own code
// controls (which a hard kill never gets to run at all).
var (
	modkernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = modkernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = modkernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess              = modkernel32.NewProc("OpenProcess")
)

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x2000
	processSetQuota                        = 0x0100
	processTerminate                       = 0x0001
)

// These mirror JOBOBJECT_BASIC_LIMIT_INFORMATION / JOBOBJECT_EXTENDED_LIMIT_INFORMATION
// from the Windows SDK closely enough for SetInformationJobObject to accept
// them — every field has to be present in the right order/size for the
// struct layout to match what the API expects, even the ones this code
// never reads or sets.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	jobOnce   sync.Once
	jobHandle syscall.Handle
)

func sharedJobObject() syscall.Handle {
	jobOnce.Do(func() {
		h, _, _ := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		handle := syscall.Handle(h)
		info := jobObjectExtendedLimitInformation{
			BasicLimitInformation: jobObjectBasicLimitInformation{
				LimitFlags: jobObjectLimitKillOnJobClose,
			},
		}
		ret, _, _ := procSetInformationJobObject.Call(
			uintptr(handle),
			uintptr(jobObjectExtendedLimitInformationClass),
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
		)
		if ret == 0 {
			syscall.CloseHandle(handle)
			return
		}
		jobHandle = handle
	})
	return jobHandle
}

// assignToJobObject ties process to the shared job object so it's killed
// automatically the moment this app's own process goes away. Best-effort:
// any failure here just means the old (manual-cleanup-only) behavior —
// never a reason to fail the caller's actual request.
func assignToJobObject(process *os.Process) {
	if process == nil {
		return
	}
	h := sharedJobObject()
	if h == 0 {
		return
	}
	procHandle, _, _ := procOpenProcess.Call(
		uintptr(processSetQuota|processTerminate),
		0,
		uintptr(process.Pid),
	)
	if procHandle == 0 {
		return
	}
	defer syscall.CloseHandle(syscall.Handle(procHandle))
	procAssignProcessToJobObject.Call(uintptr(h), procHandle)
}
