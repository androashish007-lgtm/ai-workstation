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
	"net"
	"net/http"
	"os/exec"
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

	if err := waitHealthy(ctx, tp.baseURL+"/health", 90*time.Second, exited); err != nil {
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

// waitHealthy polls /health until it responds OK, the process exits (fast
// failure — e.g. a corrupt model file), or timeout elapses.
func waitHealthy(ctx context.Context, url string, timeout time.Duration, exited <-chan error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
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
