package engine

import (
	"os"
	"runtime"
	"strings"

	"aistation/internal/hw"
)

// IsTermux detects Termux specifically (as opposed to a "real" Linux host),
// since GOOS is just "linux" either way for a CGO_ENABLED=0 build — Termux
// sets these environment variables itself, so this is the standard way
// Termux apps distinguish themselves from other Linux environments.
func IsTermux() bool {
	if strings.Contains(os.Getenv("PREFIX"), "com.termux") {
		return true
	}
	return os.Getenv("TERMUX_VERSION") != ""
}

// PlatformKey names the engines/<key>/ subdirectory used for this host.
func PlatformKey() string {
	if IsTermux() {
		return "termux-" + runtime.GOARCH
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

// Backend picks which accelerated build to prefer, based on best-effort GPU
// detection. "cpu" is always a safe fallback.
type Backend string

const (
	BackendCPU    Backend = "cpu"
	BackendVulkan Backend = "vulkan"
	BackendMetal  Backend = "metal" // macOS builds bundle Metal support by default, no separate asset
)

// PreferredBackend deliberately prefers Vulkan over vendor-specific CUDA/ROCm
// builds whenever any dedicated GPU is present and Vulkan is available:
// Vulkan accelerates NVIDIA/AMD/Intel alike from a single asset, with no need
// to match a CUDA/ROCm build to whatever driver version happens to be
// installed. That driver-matching problem is exactly the kind of thing that
// would otherwise need a user-facing choice, which conflicts with "zero
// exposed parameters" — so we trade a little peak performance on NVIDIA for
// an automatic download that's reliable on every GPU vendor.
func PreferredBackend(profile hw.Profile) Backend {
	switch {
	case profile.GPUVendor == hw.GPUApple:
		return BackendMetal
	case profile.HasVulkan && profile.GPUVendor != hw.GPUNone:
		return BackendVulkan
	default:
		return BackendCPU
	}
}
