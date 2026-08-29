// Package hw detects host hardware (RAM, CPU, GPU) with no cgo and no external
// dependencies beyond golang.org/x/sys, so cross-compiled static binaries keep
// working with zero runtime install. Every detector is best-effort: failure to
// detect a GPU (or anything else) degrades to a safe CPU-only assumption rather
// than an error, since hardware detection must never block startup.
package hw

import (
	"bufio"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

type GPUVendor string

const (
	GPUNone   GPUVendor = "none"
	GPUNvidia GPUVendor = "nvidia"
	GPUAMD    GPUVendor = "amd"
	GPUIntel  GPUVendor = "intel"
	GPUApple  GPUVendor = "apple"
)

// Profile is a snapshot of what this machine can run models on.
type Profile struct {
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	CPUCores      int       `json:"cpu_cores"`
	TotalRAMBytes uint64    `json:"total_ram_bytes"`
	AvailRAMBytes uint64    `json:"avail_ram_bytes"`
	GPUVendor     GPUVendor `json:"gpu_vendor"`
	GPUName       string    `json:"gpu_name"`
	VRAMBytes     uint64    `json:"vram_bytes"`
	HasVulkan     bool      `json:"has_vulkan"`
}

// Detect builds a fresh hardware profile. It is cheap enough to call on every
// request (a few syscalls / one short-lived subprocess at most), which is what
// lets routing re-check headroom before every model load.
func Detect() Profile {
	p := Profile{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUCores: runtime.NumCPU(),
	}
	p.TotalRAMBytes, p.AvailRAMBytes = detectRAM()
	p.GPUVendor, p.GPUName, p.VRAMBytes = detectGPU()
	p.HasVulkan = detectVulkan()
	return p
}

// BudgetBytes is the amount of memory we're willing to let a single model
// consume: prefer VRAM when a GPU is present (the model will be offloaded
// there), otherwise leave headroom under available system RAM so the OS and
// the orchestrator itself don't get starved.
func (p Profile) BudgetBytes() uint64 {
	if p.VRAMBytes > 0 {
		return p.VRAMBytes
	}
	avail := p.AvailRAMBytes
	if avail == 0 {
		avail = p.TotalRAMBytes
	}
	headroom := avail / 5 // keep ~20% free
	if headroom > avail {
		return 0
	}
	return avail - headroom
}

// InstallBudgetBytes is the looser hardware-fit check used when deciding
// whether to even suggest a model for download (catalog.Suggest/BestFit),
// as opposed to BudgetBytes' answer to "does this fit RIGHT NOW" for an
// actual model-load decision. A download itself only needs disk space —
// gating it on this moment's available RAM (which swings wildly with
// whatever else happens to be running) means a model that's perfectly
// fine for this hardware can stop being suggested at all just because
// something else is briefly using memory. Based on total RAM instead.
func (p Profile) InstallBudgetBytes() uint64 {
	if p.VRAMBytes > 0 {
		return p.VRAMBytes
	}
	headroom := p.TotalRAMBytes / 5 // keep ~20% of total as headroom
	if headroom > p.TotalRAMBytes {
		return 0
	}
	return p.TotalRAMBytes - headroom
}

func detectVulkan() bool {
	if _, err := exec.LookPath("vulkaninfo"); err == nil {
		return true
	}
	return false
}

// --- RAM ---

func detectRAM() (total, avail uint64) {
	switch runtime.GOOS {
	case "linux", "android":
		return readProcMeminfo()
	case "darwin":
		return readDarwinMem()
	case "windows":
		return readWindowsMem()
	}
	return 0, 0
}

func readProcMeminfo() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		var key string
		var kb uint64
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key = strings.TrimSuffix(fields[0], ":")
		kb, _ = strconv.ParseUint(fields[1], 10, 64)
		switch key {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	if avail == 0 {
		avail = total
	}
	return total, avail
}

func readDarwinMem() (total, avail uint64) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err == nil {
		if v, perr := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); perr == nil {
			total = v
		}
	}
	// vm_stat gives free+inactive pages; treat as available. Best-effort only.
	out, err = exec.Command("vm_stat").Output()
	if err == nil {
		pageSize := uint64(4096)
		freePages, inactivePages := uint64(0), uint64(0)
		re := regexp.MustCompile(`(\d+)`)
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "Pages free") {
				if m := re.FindString(line); m != "" {
					freePages, _ = strconv.ParseUint(m, 10, 64)
				}
			}
			if strings.HasPrefix(line, "Pages inactive") {
				if m := re.FindString(line); m != "" {
					inactivePages, _ = strconv.ParseUint(m, 10, 64)
				}
			}
		}
		avail = (freePages + inactivePages) * pageSize
	}
	if avail == 0 {
		avail = total
	}
	return total, avail
}
