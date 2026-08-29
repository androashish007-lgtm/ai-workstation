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
func (s *UsageSampler) Start(stop <-chan struct{}) {
	s.tick()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.tick()
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
