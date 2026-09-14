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
	"time"
)

// conservative default before any real measurement exists: assumes a modest
// modern CPU. Self-corrects (often downward, on weak/virtualized hardware)
// after the very first generation.
const defaultSecPerStepAt512 = 8.0

// defaultOverheadSeconds: conservative starting guess for the fixed cost of
// one generation beyond sampling and beyond model load — VAE decode, text
// conditioning. Model load now has its own separately-timed/enforced
// LoadTimeout (see engine.ImageRequest), so this no longer needs to budget
// for it. Self-corrects per model after the first real measurement, same as
// SecPerStepAt512 — this default only matters for a model that's never been
// generated with before.
const defaultOverheadSeconds = 45.0

type estimate struct {
	SecPerStepAt512 float64 `json:"sec_per_step_at_512"`
	Samples         int     `json:"samples"`
	OverheadSeconds float64 `json:"overhead_seconds,omitempty"`
	OverheadSamples int     `json:"overhead_samples,omitempty"`
}

// Store tracks a separate estimate per model, keyed by the registry model
// ID passed into Record/SecPerStep/StepsForBudget/HasData — NOT one single
// global number. Different image models have wildly different per-step
// compute costs even at the same resolution (a small SD1.5 UNet vs. a much
// larger FLUX-class transformer aren't remotely comparable); a single
// blended estimate gets corrupted the moment a dramatically slower model is
// tried even once. Confirmed the hard way: one FLUX.2-klein test run
// (~9x slower per step than this machine's SD1.5 checkpoints) nearly
// doubled what had been a well-calibrated global estimate, which would have
// silently under-stepped every fast checkpoint used afterward.
type Store struct {
	mu   sync.Mutex
	path string
	byID map[string]*estimate
}

func New(dataDir string) *Store {
	s := &Store{path: filepath.Join(dataDir, "image-perf.json"), byID: map[string]*estimate{}}
	s.load()
	return s
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var m map[string]*estimate
	// A file from before per-model tracking was a single flat {sec_per_step_at_512,
	// samples} object, not a map — it fails this Unmarshal (wrong shape) and
	// is simply discarded rather than migrated: this is a self-healing perf
	// cache the app already treats as freely re-calibratable, not data worth
	// writing migration code for.
	if err := json.Unmarshal(b, &m); err == nil && m != nil {
		s.byID = m
	}
}

func (s *Store) save() {
	b, err := json.MarshalIndent(s.byID, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.path)
	}
}

// Record folds in one real measurement for modelID: secPerStep observed
// while generating at width x height. Normalized to a 512x512-equivalent
// so future estimates at any resolution for this same model can scale from
// one number. Uses an exponential moving average so the estimate adapts to
// changing conditions (thermal throttling) without one outlier sample
// swinging it wildly.
func (s *Store) Record(modelID string, width, height int, secPerStep float64) {
	if secPerStep <= 0 || width <= 0 || height <= 0 || modelID == "" {
		return
	}
	pixelRatio := float64(width*height) / (512.0 * 512.0)
	normalized := secPerStep / pixelRatio

	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[modelID]
	if !ok {
		e = &estimate{}
		s.byID[modelID] = e
	}
	if e.Samples == 0 {
		e.SecPerStepAt512 = normalized
	} else {
		const alpha = 0.4
		e.SecPerStepAt512 = e.SecPerStepAt512*(1-alpha) + normalized*alpha
	}
	e.Samples++
	s.save()
}

// RecordOverhead folds in one real measurement of the non-sampling,
// non-load cost for modelID: given the attempt's total wall-clock time, how
// long the separately-timed load phase took (loadDuration — model load is
// now bounded by its own LoadTimeout and tracked via ImageResult.LoadDuration,
// so it's subtracted here rather than blended in), and how many sampling
// steps it actually completed (reached, from ParseStepsReached — recorded
// on a timed-out/failed attempt just as much as a successful one, since a
// near-miss that got killed one step from done is exactly the case this
// exists to fix), the sampling time implied by the already-tracked
// per-step estimate is subtracted off too; whatever's left is pure
// decode/conditioning overhead. Same exponential-moving-average approach as
// Record, independent sample count since the two measurements aren't
// always available from the same run.
func (s *Store) RecordOverhead(modelID string, width, height int, totalElapsed, loadDuration time.Duration, stepsReached int) {
	if modelID == "" || stepsReached <= 0 || totalElapsed <= 0 || width <= 0 || height <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[modelID]
	if !ok {
		e = &estimate{}
		s.byID[modelID] = e
	}
	secPerStep := e.SecPerStepAt512
	if e.Samples == 0 {
		secPerStep = defaultSecPerStepAt512
	}
	pixelRatio := float64(width*height) / (512.0 * 512.0)
	stepTime := time.Duration(float64(stepsReached) * secPerStep * pixelRatio * float64(time.Second))
	overhead := (totalElapsed - loadDuration - stepTime).Seconds()
	if overhead < 0 {
		overhead = 0
	}
	if e.OverheadSamples == 0 {
		e.OverheadSeconds = overhead
	} else {
		const alpha = 0.4
		e.OverheadSeconds = e.OverheadSeconds*(1-alpha) + overhead*alpha
	}
	e.OverheadSamples++
	s.save()
}

// Overhead returns the current estimated fixed non-sampling cost (model
// load, VAE decode, text conditioning) for this model — the conservative
// default if it has no real measurement yet.
func (s *Store) Overhead(modelID string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[modelID]
	if !ok || e.OverheadSamples == 0 {
		return defaultOverheadSeconds
	}
	return e.OverheadSeconds
}

// HasData reports whether any real measurement has ever been recorded for
// this specific model — false means its estimate is still the conservative
// built-in guess, which callers can use to decide whether a cheap
// calibration probe is worth running before committing to a full-length
// attempt.
func (s *Store) HasData(modelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[modelID]
	return ok && e.Samples > 0
}

// SecPerStep returns the current estimated seconds-per-sampling-step for
// this model at the given resolution — the conservative default if this
// model has no real measurement yet.
func (s *Store) SecPerStep(modelID string, width, height int) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	pixelRatio := float64(width*height) / (512.0 * 512.0)
	e, ok := s.byID[modelID]
	if !ok || e.Samples == 0 {
		return defaultSecPerStepAt512 * pixelRatio
	}
	return e.SecPerStepAt512 * pixelRatio
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
// the given resolution, leaving room for this model's own measured (or, if
// unmeasured, conservatively guessed) fixed overhead — model load, VAE
// decode, text conditioning — clamped to a [minSteps, maxSteps] range so it
// never degrades below a usable floor or spends steps past the point of
// meaningful quality return.
func (s *Store) StepsForBudget(modelID string, width, height int, budgetSeconds float64, minSteps, maxSteps int) int {
	perStep := s.SecPerStep(modelID, width, height) * safetyMargin
	if perStep <= 0 {
		perStep = defaultSecPerStepAt512
	}
	steps := int((budgetSeconds - s.Overhead(modelID)) / perStep)
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

var stepCountRe = regexp.MustCompile(`(\d+)/(\d+)\s*-\s*[\d.]+s/it`)

// ParseStepsReached extracts the last "current/total" step counter sd-cli
// logged, so RecordOverhead can tell how much of the sampling phase
// actually completed — deliberately works on a killed/timed-out run's
// output just as much as a successful one's, since a near-miss that got
// killed one step short of done (confirmed the real failure mode this
// exists to fix) is exactly the case whose overhead needs correcting.
func ParseStepsReached(output string) (current, total int, ok bool) {
	matches := stepCountRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return 0, 0, false
	}
	last := matches[len(matches)-1]
	cur, err1 := strconv.Atoi(last[1])
	tot, err2 := strconv.Atoi(last[2])
	if err1 != nil || err2 != nil || cur <= 0 {
		return 0, 0, false
	}
	return cur, tot, true
}
