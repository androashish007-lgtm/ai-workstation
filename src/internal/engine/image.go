// Image engine: stable-diffusion.cpp ships as a CLI, not a persistent
// server, so each generation request spawns it once with the model, prompt,
// and inferred size/steps, waits for the PNG it writes, and reads that back.
// (A persistent/keep-warm server mode is a plausible Phase 2 optimization
// once the CLI-per-request path is proven reliable across platforms.)
package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

type ImageRequest struct {
	BinPath   string
	ModelPath string
	Prompt    string
	Width     int
	Height    int
	Steps     int
	Threads   int // 0 = let ImageRequest.Threads default to all logical CPUs
	TmpDir    string
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
	args := []string{
		"-m", req.ModelPath,
		"-p", req.Prompt,
		"-o", outPath,
		"--width", strconv.Itoa(req.Width),
		"--height", strconv.Itoa(req.Height),
		"--steps", strconv.Itoa(req.Steps),
		"--threads", strconv.Itoa(threads),
	}
	cmd := exec.CommandContext(ctx, req.BinPath, args...)
	out, err := cmd.CombinedOutput()
	result := ImageResult{RawOutput: string(out)}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
