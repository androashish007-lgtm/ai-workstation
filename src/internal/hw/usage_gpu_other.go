//go:build !windows

package hw

// sampleGPUPercent has no cross-platform equivalent implemented yet
// (Linux would mean per-vendor tools — intel_gpu_top, radeontop,
// nvidia-smi; macOS has no simple CLI equivalent either). The status
// widget just shows GPU vendor/name without a live percentage on these
// platforms rather than guessing.
func sampleGPUPercent() (float64, bool) {
	return 0, false
}
