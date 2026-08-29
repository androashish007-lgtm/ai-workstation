package hw

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// detectGPU is entirely best-effort: it shells out to vendor tools that may or
// may not be on PATH, and any failure silently falls back to "no GPU" so a
// missing tool never blocks startup or crashes detection.
func detectGPU() (GPUVendor, string, uint64) {
	if v, name, vram, ok := detectNvidia(); ok {
		return v, name, vram
	}
	if runtime.GOOS == "darwin" {
		if v, name, vram, ok := detectAppleSilicon(); ok {
			return v, name, vram
		}
	}
	if v, name, vram, ok := detectAMDOrIntel(); ok {
		return v, name, vram
	}
	return GPUNone, "", 0
}

func detectNvidia() (GPUVendor, string, uint64, bool) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return "", "", 0, false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", "", 0, false
	}
	parts := strings.Split(lines[0], ",")
	if len(parts) < 2 {
		return "", "", 0, false
	}
	name := strings.TrimSpace(parts[0])
	mib, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
	return GPUNvidia, name, mib * 1024 * 1024, true
}

func detectAppleSilicon() (GPUVendor, string, uint64, bool) {
	out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err != nil {
		return "", "", 0, false
	}
	brand := strings.TrimSpace(string(out))
	if !strings.Contains(brand, "Apple") {
		// Intel Mac: no unified-memory GPU to report here.
		return "", "", 0, false
	}
	// Apple Silicon: GPU shares system RAM (unified memory), so VRAM is
	// reported as 0 and callers should fall back to system RAM budgeting.
	return GPUApple, brand + " (Metal)", 0, true
}

func detectAMDOrIntel() (GPUVendor, string, uint64, bool) {
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("wmic", "path", "win32_VideoController", "get", "name,AdapterRAM").Output()
		if err != nil {
			return "", "", 0, false
		}
		text := string(out)
		vendor := GPUNone
		if strings.Contains(strings.ToUpper(text), "AMD") || strings.Contains(strings.ToUpper(text), "RADEON") {
			vendor = GPUAMD
		} else if strings.Contains(strings.ToUpper(text), "INTEL") {
			vendor = GPUIntel
		}
		if vendor == GPUNone {
			return "", "", 0, false
		}
		return vendor, "", 0, true
	case "linux", "android":
		out, err := exec.Command("lspci").Output()
		if err != nil {
			return "", "", 0, false
		}
		text := strings.ToUpper(string(out))
		if strings.Contains(text, "AMD") || strings.Contains(text, "RADEON") {
			return GPUAMD, "", 0, true
		}
		if strings.Contains(text, "INTEL") && strings.Contains(text, "VGA") {
			return GPUIntel, "", 0, true
		}
	}
	return "", "", 0, false
}
