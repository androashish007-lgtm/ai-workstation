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
	Threads  int     // 0 = let ImageRequest.Threads default to all logical CPUs
	TmpDir   string
}

// ImageResult always carries RawOutput — the CLI's own stderr/stdout log —
// even on failure/timeout, since it's the only source of real per-step
// timing (used to self-calibrate future step counts to this machine's
// actual measured speed).
type ImageResult struct {
	PNG       []byte
	RawOutput string
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
	// happening.
	label := filepath.Base(req.ModelPath)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				step, total := pw.progress()
				if total > 0 {
					log.Printf("generating with %s: step %d/%d", label, step, total)
				} else {
					log.Printf("generating with %s...", label)
				}
			}
		}
	}()

	err := cmd.Wait()
	close(done)

	result := ImageResult{RawOutput: pw.String()}
	if err != nil {
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
