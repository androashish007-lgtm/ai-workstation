// Live CPU/RAM usage sampling for the small always-on counter in the UI.
// Unlike Detect() (a point-in-time hardware profile), CPU load is
// inherently a delta measurement — we keep a background sampler that ticks
// once a second and exposes the latest reading instantly, so the API never
// blocks a request on a sampling window. Each platform file implements
// sampleCPUPercent() and manages its own internal delta state (only ever
// called from this single sampler goroutine, so no locking needed there).
package hw

import (
	"sync"
	"time"
)

type Usage struct {
	CPUPercent    float64 `json:"cpu_percent"`
	RAMPercent    float64 `json:"ram_percent"`
	RAMUsedBytes  uint64  `json:"ram_used_bytes"`
	RAMTotalBytes uint64  `json:"ram_total_bytes"`
	CPUAvailable  bool    `json:"cpu_available"` // false until the first valid CPU sample (or if unsupported); RAM is still filled in
	GPUPercent    float64 `json:"gpu_percent"`
	GPUAvailable  bool    `json:"gpu_available"` // false until the first valid GPU sample (or if unsupported on this OS)
}

type UsageSampler struct {
	mu     sync.RWMutex
	latest Usage
}

func NewUsageSampler() *UsageSampler {
	return &UsageSampler{}
}

func (s *UsageSampler) Latest() Usage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

// Start runs the sampling loop until stop is closed. Call it in its own
// goroutine; a failed sample just keeps the previous value rather than
// crashing the loop — usage reporting is cosmetic, never load-bearing.
//
// GPU utilization is sampled on its own slower, non-blocking ticker: unlike
// CPU/RAM, querying it (on Windows, via the same performance counter set
// Task Manager's GPU tab reads) costs on the order of a second each time —
// running it on the 1s CPU/RAM cadence would mean it's *always* running,
// which is both wasteful and would lag the CPU/RAM numbers behind it.
func (s *UsageSampler) Start(stop <-chan struct{}) {
	s.tick()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	gpuTicker := time.NewTicker(6 * time.Second)
	defer gpuTicker.Stop()
	var gpuMu sync.Mutex
	gpuInFlight := false

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.tick()
		case <-gpuTicker.C:
			gpuMu.Lock()
			if gpuInFlight {
				gpuMu.Unlock()
				continue // previous sample still running; skip this tick rather than pile up
			}
			gpuInFlight = true
			gpuMu.Unlock()
			go func() {
				defer func() {
					recover() // sampleGPUPercent shouldn't panic, but this is best-effort cosmetics — never take the app down over it
					gpuMu.Lock()
					gpuInFlight = false
					gpuMu.Unlock()
				}()
				if pct, ok := sampleGPUPercent(); ok {
					s.mu.Lock()
					s.latest.GPUPercent = pct
					s.latest.GPUAvailable = true
					s.mu.Unlock()
				}
			}()
		}
	}
}

func (s *UsageSampler) tick() {
	total, avail := detectRAM()
	used := uint64(0)
	ramPct := 0.0
	if total > 0 {
		if avail > total {
			avail = total
		}
		used = total - avail
		ramPct = float64(used) / float64(total) * 100
	}
	cpuPct, cpuOK := sampleCPUPercent()

	s.mu.Lock()
	s.latest.RAMUsedBytes = used
	s.latest.RAMTotalBytes = total
	s.latest.RAMPercent = ramPct
	if cpuOK {
		s.latest.CPUPercent = cpuPct
		s.latest.CPUAvailable = true
	}
	s.mu.Unlock()
}
