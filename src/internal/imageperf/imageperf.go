// Package imageperf self-calibrates image-generation step counts to this
// machine's actual measured speed, instead of guessing a fixed step count
// that might take 47 seconds *per step* on weak/virtualized CPU floating
// point and blow any time budget regardless of resolution. Every generation
// (successful or not — even a timed-out attempt logs partial step timing)
// feeds back a measurement, so the estimate gets more accurate with use.
package imageperf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
)

// conservative default before any real measurement exists: assumes a modest
// modern CPU. Self-corrects (often downward, on weak/virtualized hardware)
// after the very first generation.
const defaultSecPerStepAt512 = 8.0

type estimate struct {
	SecPerStepAt512 float64 `json:"sec_per_step_at_512"`
	Samples         int     `json:"samples"`
}

type Store struct {
	mu   sync.Mutex
	path string
	est  estimate
}

func New(dataDir string) *Store {
	s := &Store{path: filepath.Join(dataDir, "image-perf.json"), est: estimate{SecPerStepAt512: defaultSecPerStepAt512}}
	s.load()
	return s
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var e estimate
	if err := json.Unmarshal(b, &e); err == nil && e.SecPerStepAt512 > 0 {
		s.est = e
	}
}

func (s *Store) save() {
	b, err := json.MarshalIndent(s.est, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.path)
	}
}

// Record folds in one real measurement: secPerStep observed while
// generating at width x height. Normalized to a 512x512-equivalent so
// future estimates at any resolution can scale from one number. Uses an
// exponential moving average so the estimate adapts to changing conditions
// (thermal throttling, a different model's compute cost) without one
// outlier sample swinging it wildly.
func (s *Store) Record(width, height int, secPerStep float64) {
	if secPerStep <= 0 || width <= 0 || height <= 0 {
		return
	}
	pixelRatio := float64(width*height) / (512.0 * 512.0)
	normalized := secPerStep / pixelRatio

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.est.Samples == 0 {
		s.est.SecPerStepAt512 = normalized
	} else {
		const alpha = 0.4
		s.est.SecPerStepAt512 = s.est.SecPerStepAt512*(1-alpha) + normalized*alpha
	}
	s.est.Samples++
	s.save()
}

// HasData reports whether any real measurement has ever been recorded —
// false means every estimate so far is still the conservative built-in
// guess, which callers can use to decide whether a cheap calibration probe
// is worth running before committing to a full-length attempt.
func (s *Store) HasData() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.est.Samples > 0
}

// SecPerStep returns the current estimated seconds-per-sampling-step at the
// given resolution.
func (s *Store) SecPerStep(width, height int) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	pixelRatio := float64(width*height) / (512.0 * 512.0)
	return s.est.SecPerStepAt512 * pixelRatio
}

// safetyMargin inflates the measured per-step estimate before it's used to
// pick a step count: compute cost doesn't scale perfectly linearly with
// pixel count (attention layers scale worse than convolutions), measurement
// noise exists, and different checkpoints at the "same" resolution aren't
// identically fast. Under-committing steps and finishing successfully beats
// a precisely-computed step count that gets killed by its own timeout with
// nothing to show for it.
const safetyMargin = 1.3

// StepsForBudget returns how many sampling steps fit in budgetSeconds at
// the given resolution, leaving room for the fixed overhead every run pays
// (model load, VAE encode/decode, text conditioning) — clamped to a
// [minSteps, maxSteps] range so it never degrades below a usable floor or
// spends steps past the point of meaningful quality return.
func (s *Store) StepsForBudget(width, height int, budgetSeconds float64, minSteps, maxSteps int) int {
	const fixedOverheadSeconds = 20.0
	perStep := s.SecPerStep(width, height) * safetyMargin
	if perStep <= 0 {
		perStep = defaultSecPerStepAt512
	}
	steps := int((budgetSeconds - fixedOverheadSeconds) / perStep)
	if steps < minSteps {
		steps = minSteps
	}
	if steps > maxSteps {
		steps = maxSteps
	}
	return steps
}

var stepTimingRe = regexp.MustCompile(`\d+/\d+\s*-\s*([\d.]+)s/it`)

// ParseSecPerStep extracts the per-step timing sd-cli logs to
// stderr/stdout (e.g. "|====> | 3/15 - 12.34s/it"), taking the last match
// as most representative (later steps exclude one-time warmup). Works on
// output from a completed OR a killed/timed-out run — whatever was captured
// before the process was cut off.
func ParseSecPerStep(output string) (float64, bool) {
	matches := stepTimingRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return 0, false
	}
	last := matches[len(matches)-1]
	v, err := strconv.ParseFloat(last[1], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}
