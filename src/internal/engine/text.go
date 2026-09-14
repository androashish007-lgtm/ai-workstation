// Text engine process: launches llama-server as a long-lived local HTTP
// server and speaks its OpenAI-compatible /v1/chat/completions API,
// including SSE streaming, so the rest of the app never has to know it's
// talking to a subprocess rather than a cloud API.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aistation/internal/safego"
)

type TextProcess struct {
	cmd       *exec.Cmd
	port      int
	baseURL   string
	ModelPath string
	GPULayers int
	CtxTokens int
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// StartTextServer launches llama-server for modelPath. gpuLayers is 0 for a
// CPU-only build/profile, or a large number (we use 999) to offload as many
// layers as the backend can fit when a GPU backend is available — llama.cpp
// clamps this to what actually fits. mmprojPath, if non-empty, enables
// vision input for this model (a multimodal projector paired by the
// registry's filename-prefix heuristic). parallelSlots lets this one process
// serve that many concurrent requests (e.g. two chats sharing a model)
// without them queuing behind each other; llama.cpp splits --ctx-size across
// slots, so it's multiplied up here to preserve the intended per-conversation
// context length.
func StartTextServer(ctx context.Context, binPath, modelPath string, ctxTokens, gpuLayers int, mmprojPath string, parallelSlots int) (*TextProcess, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	if parallelSlots < 1 {
		parallelSlots = 1
	}
	args := []string{
		"--model", modelPath,
		"--port", strconv.Itoa(port),
		"--host", "127.0.0.1",
		"--ctx-size", strconv.Itoa(ctxTokens * parallelSlots),
		"--parallel", strconv.Itoa(parallelSlots),
		"--n-gpu-layers", strconv.Itoa(gpuLayers),
		"--jinja", // use each model's own chat template when present
	}
	if mmprojPath != "" {
		args = append(args, "--mmproj", mmprojPath)
	}
	cmd := exec.CommandContext(ctx, binPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting llama-server: %w", err)
	}
	assignToJobObject(cmd.Process)

	// Watched concurrently with health polling so a fast crash (e.g. a
	// corrupt/incompatible model file) fails immediately with the engine's
	// own error output, instead of silently retrying HTTP connects against
	// a dead process for the full health-check timeout.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	tp := &TextProcess{cmd: cmd, port: port, baseURL: fmt.Sprintf("http://127.0.0.1:%d", port), ModelPath: modelPath, GPULayers: gpuLayers, CtxTokens: ctxTokens}

	sizeBytes := fileSizeOrZero(modelPath)
	progress := loadProgress{pid: cmd.Process.Pid, sizeBytes: sizeBytes, label: filepath.Base(modelPath)}
	if err := waitHealthy(ctx, tp.baseURL+"/health", LoadTimeoutFor(sizeBytes), exited, progress); err != nil {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		if tail := lastLines(stderr.String(), 6); tail != "" {
			return nil, fmt.Errorf("text engine failed to start: %w\n%s", err, tail)
		}
		return nil, fmt.Errorf("text engine did not become healthy: %w", err)
	}
	return tp, nil
}

// minHealthTimeout is the floor (and the whole timeout on typical
// fast-storage hardware) — genuinely broken small models still fail this
// fast rather than hanging around for the scaled ceiling below.
const minHealthTimeout = 90 * time.Second

// maxHealthTimeout caps how long a legitimately-slow load is ever allowed
// to keep retrying the health check, so a truly broken large model doesn't
// hang around indefinitely either.
const maxHealthTimeout = 10 * time.Minute

// assumedFloorMBPerSec is a deliberately conservative disk-throughput floor
// used only to size the timeout, not to predict real speed — a fast NVMe
// load finishes in a fraction of the resulting budget and is unaffected;
// this exists so a model on much slower storage (a USB/SD-based portable
// install) isn't killed as "hung" purely for still legitimately reading a
// multi-GB file off a slow drive, which minHealthTimeout's flat 90s alone
// can't distinguish from an actually-stuck process.
const assumedFloorMBPerSec = 20.0

// LoadTimeoutFor sizes a load-phase timeout to the total bytes being
// loaded, so a large model (or set of files, for a multi-file image model)
// on slow storage gets proportionally more time before being judged stuck,
// while a small one (or anything on typical fast storage) still fails
// within minHealthTimeout if something's really wrong. sizeBytes 0 (file
// couldn't be stat'd) falls back to minHealthTimeout — the actual load is
// about to fail anyway if that's the case. Shared between the text engine's
// health-check wait and the image engine's load-phase watchdog (see
// image.go) — both are "how long until we give up on this model even
// starting," the same question for either engine.
func LoadTimeoutFor(sizeBytes int64) time.Duration {
	sizeMB := float64(sizeBytes) / (1 << 20)
	estimatedLoad := time.Duration(sizeMB / assumedFloorMBPerSec * float64(time.Second))
	timeout := estimatedLoad + 30*time.Second // startup/health-check overhead on top of the raw read
	if timeout < minHealthTimeout {
		timeout = minHealthTimeout
	}
	if timeout > maxHealthTimeout {
		timeout = maxHealthTimeout
	}
	return timeout
}

// fileSizeOrZero returns path's size, or 0 if it can't be stat'd (never an
// error a caller needs to handle specially — every use of the result
// already treats 0 as "no size information available").
func fileSizeOrZero(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// waitHealthy polls /health until it responds OK, the process exits (fast
// failure — e.g. a corrupt model file), or timeout elapses.
// loadProgress carries what a still-loading process's periodic progress
// line needs to say — see waitHealthy's progressInterval heartbeat.
type loadProgress struct {
	pid       int
	sizeBytes int64  // 0 = unknown; heartbeat then omits the percentage
	label     string // e.g. the model's filename, for the log line
}

// progressInterval: how often waitHealthy logs a heartbeat while otherwise
// silently polling — long loads on slow storage used to produce no output
// at all between "loading model" and either success or a timeout error,
// minutes later, which looked identical to a hung process from the Logs
// tab. This guarantees at least one line every 30s for as long as the wait
// continues, without adding one on top of a health check that resolves
// sooner (the 300ms poll below is what actually reacts fast to success).
const progressInterval = 30 * time.Second

func waitHealthy(ctx context.Context, url string, timeout time.Duration, exited <-chan error, progress loadProgress) error {
	deadline := time.Now().Add(timeout)
	lastProgress := time.Now()
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Since(lastProgress) >= progressInterval {
			lastProgress = time.Now()
			safeLogLoadProgress(progress, deadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case werr := <-exited:
			if werr != nil {
				return fmt.Errorf("process exited: %w", werr)
			}
			return fmt.Errorf("process exited before becoming healthy")
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out after %s", timeout)
}

// logLoadProgress reports how far a still-loading model has gotten, using
// the process's current resident memory as a proxy for bytes read so far —
// llama.cpp memory-maps the weight file, so working set tracks read
// progress closely enough to be a useful "is this actually moving" signal,
// even though it's not an exact byte-for-byte count. Falls back to a plain
// elapsed-time line if the platform-specific memory read isn't available
// (see the procmem_*.go files) or the file size wasn't known.
// safeLogLoadProgress is the only way waitHealthy ever calls
// logLoadProgress — this is a nice-to-have log line, not something that
// should ever get a chance to threaten the actual generation it's running
// alongside (see processRSSBytes's Windows implementation for the incident
// that made this the rule rather than a hypothetical).
func safeLogLoadProgress(p loadProgress, deadline time.Time) {
	defer func() { recover() }()
	logLoadProgress(p, deadline)
}

func logLoadProgress(p loadProgress, deadline time.Time) {
	// See image.go's deadlineNote for why this is only ever a log line, not
	// something the app acts on — the timeout itself already scales with
	// this model's file size (see healthTimeoutFor), this just makes how
	// much of that budget is left visible while it's still running instead
	// of only finding out when it either succeeds or times out.
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	deadlineSuffix := fmt.Sprintf(" — %s left before this load is given up on", remaining.Round(time.Second))

	rss, ok := processRSSBytes(p.pid)
	if !ok || p.sizeBytes <= 0 {
		log.Printf("still loading %s...%s", p.label, deadlineSuffix)
		return
	}
	pct := float64(rss) / float64(p.sizeBytes) * 100
	if pct > 100 {
		pct = 100
	}
	log.Printf("loading %s: ~%.0f%% (%.1f GB / %.1f GB)%s", p.label, pct,
		float64(rss)/(1<<30), float64(p.sizeBytes)/(1<<30), deadlineSuffix)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (p *TextProcess) Stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
}

// ChatMessage is one turn. Images holds data: URIs; when non-empty the
// message is marshaled as an OpenAI-style multimodal content array instead
// of a plain string, which requires the server to have been started with
// --mmproj.
type ChatMessage struct {
	Role    string
	Content string
	Images  []string
}

type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

func (m ChatMessage) MarshalJSON() ([]byte, error) {
	if len(m.Images) == 0 {
		return json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{m.Role, m.Content})
	}
	parts := make([]contentPart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, contentPart{Type: "text", Text: m.Content})
	}
	for _, uri := range m.Images {
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURLPart{URL: uri}})
	}
	return json.Marshal(struct {
		Role    string        `json:"role"`
		Content []contentPart `json:"content"`
	}{m.Role, parts})
}

type chatRequest struct {
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
}

type StreamEvent struct {
	Delta string
	Done  bool
	Err   error
}

// ChatStream posts to llama-server's OpenAI-compatible endpoint and yields
// content deltas as they arrive over SSE.
func (p *TextProcess) ChatStream(ctx context.Context, messages []ChatMessage, maxTokens int, temperature float64) (<-chan StreamEvent, error) {
	body, err := json.Marshal(chatRequest{Messages: messages, Stream: true, MaxTokens: maxTokens, Temperature: temperature})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("text engine returned %d: %s", resp.StatusCode, string(b))
	}

	out := make(chan StreamEvent, 8)
	safego.Go(func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				out <- StreamEvent{Done: true}
				return
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			for _, c := range chunk.Choices {
				if c.Delta.Content != "" {
					select {
					case out <- StreamEvent{Delta: c.Delta.Content}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			out <- StreamEvent{Err: err}
		}
	})
	return out, nil
}
