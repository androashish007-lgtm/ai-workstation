// Image engine: stable-diffusion.cpp ships as a CLI, not a persistent
// server, so each generation request spawns it once with the model, prompt,
// and inferred size/steps, waits for the PNG it writes, and reads that back.
// (A persistent/keep-warm server mode is a plausible Phase 2 optimization
// once the CLI-per-request path is proven reliable across platforms.)
package engine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type ImageRequest struct {
	BinPath   string
	ModelPath string
	// VAEPath/LLMPath: set only for a FLUX.2 checkpoint, which
	// stable-diffusion.cpp can't load as a single -m file — it needs the
	// diffusion model, VAE, and text-encoder LLM as three separate files
	// passed via --diffusion-model/--vae/--llm instead. Every other family
	// leaves these empty and uses the plain -m path unchanged.
	VAEPath  string
	LLMPath  string
	Prompt   string
	Width    int
	Height   int
	Steps    int
	CFGScale float64 // 0 = omit --cfg-scale, let sd-cli use its own default
	// LoadTimeout/GenTimeout are two independent clocks this function
	// enforces itself (via its own watchdog, not just the passed-in ctx):
	// LoadTimeout bounds how long we wait for the first sampling step to
	// appear at all; GenTimeout, starting fresh the moment that first step
	// is seen, bounds how long sampling+decode is then allowed to take.
	// Confirmed the hard way why they can't be one blended number: a large
	// multi-file model's load time was counting against the same clock as
	// its generation time, so a slow load left an attempt no real chance
	// to finish even when generation itself was on pace — and separately,
	// blending load+decode into one "overhead" guess still under-reserved
	// time for decode specifically, killing attempts that had already
	// finished every sampling step. 0 disables that phase's enforcement
	// (falls back to whatever the passed-in ctx's own deadline provides,
	// if any).
	LoadTimeout time.Duration
	GenTimeout  time.Duration
	// InitImagePath, when set, switches this from a from-scratch generation
	// to editing/transforming that image via sd-cli's -i/--init-img — the
	// classic SDEdit mechanism (renoise the input by --strength, then
	// denoise): appropriate for pix2pix-style and generic SD1.x/SDXL img2img.
	// See router.InferEditParams for how Strength/ImgCFGScale are chosen
	// alongside it. Mutually exclusive with ReferenceImagePath in practice
	// (a given model uses one mechanism or the other) — set at most one.
	InitImagePath string
	Strength      float64 // 0 = omit --strength, let sd-cli use its own default (only meaningful with InitImagePath)
	ImgCFGScale   float64 // 0 = omit --img-cfg-scale (only meaningful with InitImagePath)
	// ReferenceImagePath, when set, uses sd-cli's -r/--ref-image instead —
	// FLUX Kontext/FLUX.2's own reference-image conditioning, a completely
	// different mechanism from -i's noise-based img2img (confirmed the hard
	// way: pointing a FLUX.2 edit request at -i produced an image that
	// resembled the input but ignored the text instruction entirely — -i
	// triggers SDEdit-style renoising, which has no concept of "follow this
	// instruction," whereas -r is FLUX's actual instruction-following edit
	// path). No strength/img-cfg-scale concept applies here.
	ReferenceImagePath string
	Threads            int // 0 = let ImageRequest.Threads default to all logical CPUs
	TmpDir             string
}

// ImageResult always carries RawOutput — the CLI's own stderr/stdout log —
// even on failure/timeout, since it's the only source of real per-step
// timing (used to self-calibrate future step counts to this machine's
// actual measured speed).
type ImageResult struct {
	PNG       []byte
	RawOutput string
	// LoadDuration is how long it took to reach the first sampling step —
	// zero if that was never reached (killed/failed during loading). Lets
	// the caller calibrate load cost and decode/generation cost as two
	// separate numbers instead of one blended "overhead" guess.
	LoadDuration time.Duration
	// TimedOutPhase is "load" or "generation" if this call's own watchdog
	// killed the process for exceeding LoadTimeout/GenTimeout — empty
	// otherwise (including when the passed-in ctx's own cancellation, e.g.
	// the user clicking Stop, ended it instead). Lets the caller report
	// precisely what happened rather than a generic failure.
	TimedOutPhase string
}

// GenerateImage runs one text-to-image generation and returns the resulting
// PNG bytes. Timeout scales with step count/resolution since CPU-only
// generation on large models can legitimately take minutes.
func GenerateImage(ctx context.Context, req ImageRequest) (ImageResult, error) {
	if err := os.MkdirAll(req.TmpDir, 0o755); err != nil {
		return ImageResult{}, err
	}
	outPath := filepath.Join(req.TmpDir, fmt.Sprintf("gen-%d.png", time.Now().UnixNano()))
	defer os.Remove(outPath)

	threads := req.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	var args []string
	if req.VAEPath != "" || req.LLMPath != "" {
		// FLUX.2's three-file invocation (see ImageRequest.VAEPath's doc
		// comment) — --diffusion-fa (flash attention) is recommended by
		// stable-diffusion.cpp's own docs for this path since the extra LLM
		// text encoder adds meaningful memory pressure on top of the
		// transformer.
		args = []string{"--diffusion-model", req.ModelPath, "--diffusion-fa"}
		if req.VAEPath != "" {
			args = append(args, "--vae", req.VAEPath)
		}
		if req.LLMPath != "" {
			args = append(args, "--llm", req.LLMPath)
		}
	} else {
		args = []string{"-m", req.ModelPath}
	}
	args = append(args,
		"-p", req.Prompt,
		"-o", outPath,
		"--width", strconv.Itoa(req.Width),
		"--height", strconv.Itoa(req.Height),
		"--steps", strconv.Itoa(req.Steps),
		"--threads", strconv.Itoa(threads),
	)
	if req.CFGScale > 0 {
		args = append(args, "--cfg-scale", strconv.FormatFloat(req.CFGScale, 'f', -1, 64))
	}
	if req.InitImagePath != "" {
		args = append(args, "-i", req.InitImagePath)
		if req.Strength > 0 {
			args = append(args, "--strength", strconv.FormatFloat(req.Strength, 'f', -1, 64))
		}
		if req.ImgCFGScale > 0 {
			args = append(args, "--img-cfg-scale", strconv.FormatFloat(req.ImgCFGScale, 'f', -1, 64))
		}
	}
	if req.ReferenceImagePath != "" {
		args = append(args, "-r", req.ReferenceImagePath)
	}
	cmd := exec.CommandContext(ctx, req.BinPath, args...)
	pw := &progressWriter{}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return ImageResult{}, fmt.Errorf("starting image engine: %w", err)
	}

	// Heartbeat every progressInterval (same 30s constant text.go's model
	// loading uses) so a slow generation shows real, moving step progress
	// in the Logs tab instead of going completely silent until it either
	// finishes or fails — previously CombinedOutput() blocked until the
	// process exited, so sd-cli's own step-by-step output (parsed below)
	// was only ever visible after the fact, never while it was actually
	// happening. The same tick also enforces LoadTimeout/GenTimeout: this
	// is deliberately the actual enforcement mechanism for those two
	// phases, not just logging — ctx's own cancellation (Stop button, or a
	// generous outer safety-net deadline the caller may still apply) is
	// the only other thing that can end this process early. Checking once
	// per 30s tick rather than continuously means a phase timeout can
	// overshoot by up to ~30s before being caught — an acceptable trade
	// for these being minutes-scale budgets, not a source of extra
	// complexity for sub-second precision nobody needs here.
	label := filepath.Base(req.ModelPath)
	startedAt := time.Now()
	done := make(chan struct{})
	var timedOutMu sync.Mutex
	var timedOutPhase string
	go func() {
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				step, total := pw.progress()
				firstStep := pw.firstStep()
				log.Printf("generating with %s%s", label, phaseNote(req, startedAt, firstStep, step, total))

				var timedOut string
				if firstStep.IsZero() {
					if req.LoadTimeout > 0 && time.Since(startedAt) > req.LoadTimeout {
						timedOut = "load"
					}
				} else if req.GenTimeout > 0 && time.Since(firstStep) > req.GenTimeout {
					timedOut = "generation"
				}
				if timedOut != "" {
					timedOutMu.Lock()
					timedOutPhase = timedOut
					timedOutMu.Unlock()
					cmd.Process.Kill()
					return
				}
			}
		}
	}()

	err := cmd.Wait()
	close(done)

	timedOutMu.Lock()
	finalTimedOutPhase := timedOutPhase
	timedOutMu.Unlock()
	result := ImageResult{RawOutput: pw.String(), TimedOutPhase: finalTimedOutPhase}
	if fs := pw.firstStep(); !fs.IsZero() {
		result.LoadDuration = fs.Sub(startedAt)
	}
	if err != nil {
		if finalTimedOutPhase != "" {
			return result, fmt.Errorf("%s phase exceeded its own timeout and was stopped\n%s", finalTimedOutPhase, truncate(result.RawOutput, 2000))
		}
		return result, fmt.Errorf("image generation failed: %w\n%s", err, truncate(result.RawOutput, 2000))
	}
	png, err := os.ReadFile(outPath)
	if err != nil {
		return result, fmt.Errorf("image engine reported success but produced no output: %w", err)
	}
	result.PNG = png
	return result, nil
}

// imageStepRe matches sd-cli's own per-step progress line, e.g.
// "|====================> | 3/15 - 12.34s/it" — the same shape
// imageperf.ParseSecPerStep looks for in the completed output, but this
// also captures the step numbers (not just the timing) for progressWriter's
// live heartbeat, which needs to know how far along a still-running
// generation is, not just how fast each step took once it's over.
var imageStepRe = regexp.MustCompile(`(\d+)/(\d+)\s*-\s*[\d.]+s/it`)

// progressWriter is sd-cli's combined stdout+stderr destination: it
// appends everything to buf (preserving the exact full output the caller
// used to get from CombinedOutput(), used for imageperf.ParseSecPerStep and
// error reporting) while also tracking the latest step/total it's seen, so
// GenerateImage's heartbeat goroutine can report real progress instead of
// just "still running" — sd-cli has no other progress channel to read
// short of parsing its own human-readable step lines as they arrive.
type progressWriter struct {
	mu                  sync.Mutex
	buf                 bytes.Buffer
	lastStep, lastTotal int
	// firstStepAt marks the moment the load phase ended and generation
	// began — the dividing line GenerateImage's watchdog uses to decide
	// which of LoadTimeout/GenTimeout currently applies.
	firstStepAt time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	// Match against a trailing window of the whole accumulated buffer, not
	// just this Write call's own bytes — sd-cli's output isn't guaranteed
	// to arrive one line per Write (a step line split across two calls
	// would never match if each were checked in isolation), the same
	// reason imageperf.ParseSecPerStep matches over the full final output
	// rather than per-chunk. 4KB is comfortably more than one progress
	// line ever needs.
	tail := w.buf.Bytes()
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	if matches := imageStepRe.FindAllSubmatch(tail, -1); len(matches) > 0 {
		m := matches[len(matches)-1]
		if cur, err := strconv.Atoi(string(m[1])); err == nil && cur > 0 {
			if w.firstStepAt.IsZero() {
				w.firstStepAt = time.Now()
			}
			total, _ := strconv.Atoi(string(m[2]))
			w.lastStep, w.lastTotal = cur, total
		}
	}
	w.mu.Unlock()
	return len(p), nil
}

func (w *progressWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *progressWriter) progress() (step, total int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastStep, w.lastTotal
}

// firstStep returns when the first sampling step was observed — the zero
// time if generation hasn't started yet (still loading).
func (w *progressWriter) firstStep() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.firstStepAt
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// phaseNote builds the trailing part of a progress heartbeat line, showing
// both of GenerateImage's own timers so a slower-than-expected run is
// visible as it's happening instead of only being discovered when it
// fails — without a mid-run prompt/pause that would fight the rest of this
// app's "chats keep generating in the background regardless of what's
// being looked at" design. While still loading, it shows the countdown on
// the load timeout; once the first sampling step has been seen, it shows
// how long loading actually took plus the countdown on the generation
// timeout, with a pace projection flagged explicitly when it exceeds what's
// left.
func phaseNote(req ImageRequest, startedAt, firstStep time.Time, step, total int) string {
	if firstStep.IsZero() {
		if req.LoadTimeout <= 0 {
			return "... (loading)"
		}
		remaining := req.LoadTimeout - time.Since(startedAt)
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("... (loading) — %s left on the %s load timeout", remaining.Round(time.Second), req.LoadTimeout.Round(time.Second))
	}
	loadTook := firstStep.Sub(startedAt)
	var note string
	if req.GenTimeout > 0 {
		remaining := req.GenTimeout - time.Since(firstStep)
		if remaining < 0 {
			remaining = 0
		}
		note = fmt.Sprintf(" — loaded in %s, %s left on the %s generation timeout", loadTook.Round(time.Second), remaining.Round(time.Second), req.GenTimeout.Round(time.Second))
		if step > 0 && total > 0 {
			perStep := time.Since(firstStep) / time.Duration(step)
			projected := perStep * time.Duration(total-step)
			if projected > remaining {
				note += fmt.Sprintf(", but ~%s more needed at this pace — likely to miss it and retry smaller", projected.Round(time.Second))
			} else {
				note += fmt.Sprintf(", ~%s more needed at this pace", projected.Round(time.Second))
			}
		}
	} else {
		note = fmt.Sprintf(" — loaded in %s", loadTook.Round(time.Second))
	}
	if total > 0 {
		return fmt.Sprintf(": step %d/%d%s", step, total, note)
	}
	return "..." + note
}
