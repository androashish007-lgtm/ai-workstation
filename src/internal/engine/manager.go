// Manager owns the lifecycle of both engine binaries: detecting what's
// already installed, planning what needs to be fetched/built, gating that on
// a single explicit approval (terminal prompt or UI button — whichever
// happens first), and running the download/build/extract once approved.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aistation/internal/download"
	"aistation/internal/hw"
)

type Status string

const (
	StatusMissing          Status = "missing"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusWorking          Status = "working" // downloading and/or building
	StatusReady            Status = "ready"
	StatusError            Status = "error"
)

type ComponentState struct {
	Status       Status            `json:"status"`
	Plan         *PlannedDownload  `json:"plan,omitempty"`
	Progress     download.Progress `json:"progress"`
	Message      string            `json:"message,omitempty"`
	BinaryPath   string            `json:"-"`
	WorkingSince time.Time         `json:"working_since,omitempty"` // when Status last transitioned into StatusWorking
}

type Manager struct {
	mu          sync.Mutex
	enginesRoot string
	profile     hw.Profile

	Text  ComponentState
	Image ComponentState

	approveText  chan struct{}
	approveImage chan struct{}
	textOnce     sync.Once
	imageOnce    sync.Once

	onUpdate func()
	onLog    func(string)
}

func NewManager(enginesRoot string, profile hw.Profile) *Manager {
	return &Manager{
		enginesRoot:  enginesRoot,
		profile:      profile,
		approveText:  make(chan struct{}),
		approveImage: make(chan struct{}),
		onUpdate:     func() {},
		onLog:        func(string) {},
	}
}

func (m *Manager) OnUpdate(fn func())    { m.onUpdate = fn }
func (m *Manager) OnLog(fn func(string)) { m.onLog = fn }

func (m *Manager) setText(s ComponentState) {
	m.mu.Lock()
	if s.Status == StatusWorking {
		if m.Text.Status == StatusWorking && !m.Text.WorkingSince.IsZero() {
			s.WorkingSince = m.Text.WorkingSince
		} else {
			s.WorkingSince = time.Now()
		}
	}
	m.Text = s
	m.mu.Unlock()
	m.onUpdate()
}

func (m *Manager) setImage(s ComponentState) {
	m.mu.Lock()
	if s.Status == StatusWorking {
		if m.Image.Status == StatusWorking && !m.Image.WorkingSince.IsZero() {
			s.WorkingSince = m.Image.WorkingSince
		} else {
			s.WorkingSince = time.Now()
		}
	}
	m.Image = s
	m.mu.Unlock()
	m.onUpdate()
}

func (m *Manager) Snapshot() (ComponentState, ComponentState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Text, m.Image
}

// ApproveText/ApproveImage unblock a pending bootstrap once, from whichever
// approval path (terminal stdin or the UI's approve button) fires first.
func (m *Manager) ApproveText()  { m.textOnce.Do(func() { close(m.approveText) }) }
func (m *Manager) ApproveImage() { m.imageOnce.Do(func() { close(m.approveImage) }) }

// EnsureText detects an already-installed text engine binary or, if missing,
// walks it through plan -> await approval -> fetch -> extract.
func (m *Manager) EnsureText(ctx context.Context) (string, error) {
	if p, err := FindBinary(filepath.Join(m.enginesRoot, PlatformKey(), "text"), "llama-server"); err == nil {
		m.setText(ComponentState{Status: StatusReady, BinaryPath: p})
		return p, nil
	}
	plan, err := PlanLlamaCpp(m.profile)
	if err != nil {
		m.setText(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	m.setText(ComponentState{Status: StatusAwaitingApproval, Plan: &plan,
		Message: fmt.Sprintf("Download %s (%s, %s)?", plan.Component, plan.AssetName, humanBytes(plan.SizeBytes))})
	select {
	case <-m.approveText:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	m.setText(ComponentState{Status: StatusWorking, Plan: &plan, Message: "Downloading " + plan.AssetName})
	err = FetchAndExtract(m.enginesRoot, plan, "text", func(p download.Progress) {
		m.setText(ComponentState{Status: StatusWorking, Plan: &plan, Progress: p, Message: "Downloading " + plan.AssetName})
	})
	if err != nil && IsTermux() {
		// Prebuilt android-arm64 asset may not run under Termux's process
		// model even though it downloaded fine; fall back to a source build.
		m.onLog(fmt.Sprintf("prebuilt text engine failed (%v); building from source instead", err))
		return m.buildTextFromSource(ctx)
	}
	if err != nil {
		m.setText(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	bin, err := FindBinary(filepath.Join(m.enginesRoot, PlatformKey(), "text"), "llama-server")
	if err != nil {
		if IsTermux() {
			return m.buildTextFromSource(ctx)
		}
		m.setText(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	if ok, verr := verifyRuns(bin); !ok {
		if IsTermux() {
			m.onLog(fmt.Sprintf("downloaded text engine did not run (%v); building from source instead", verr))
			return m.buildTextFromSource(ctx)
		}
	}
	m.setText(ComponentState{Status: StatusReady, BinaryPath: bin})
	return bin, nil
}

func (m *Manager) buildTextFromSource(ctx context.Context) (string, error) {
	m.setText(ComponentState{Status: StatusWorking, Message: "Building llama.cpp from source on-device..."})
	buildDir, err := BuildFromSource(ctx, m.enginesRoot, "ggml-org/llama.cpp", nil, m.onLog)
	if err != nil {
		m.setText(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	bin, err := FindBinary(buildDir, "llama-server")
	if err != nil {
		m.setText(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	m.setText(ComponentState{Status: StatusReady, BinaryPath: bin})
	return bin, nil
}

// EnsureImage mirrors EnsureText for stable-diffusion.cpp. On Termux there is
// never a prebuilt asset, so it goes straight to the (still approval-gated)
// source build.
// findSDBinary tries stable-diffusion.cpp's current CLI binary name first,
// falling back to the older bare "sd" name in case a different build/version
// uses it (confirmed against a real downloaded release: current builds ship
// "sd-cli").
func findSDBinary(dir string) (string, error) {
	if p, err := FindBinary(dir, "sd-cli"); err == nil {
		return p, nil
	}
	return FindBinary(dir, "sd")
}

func (m *Manager) EnsureImage(ctx context.Context) (string, error) {
	if p, err := findSDBinary(filepath.Join(m.enginesRoot, PlatformKey(), "image")); err == nil {
		m.setImage(ComponentState{Status: StatusReady, BinaryPath: p})
		return p, nil
	}

	plan, hasPrebuilt, err := PlanStableDiffusionCpp(m.profile)
	if err != nil {
		m.setImage(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	if !hasPrebuilt {
		m.setImage(ComponentState{Status: StatusAwaitingApproval,
			Message: "No prebuilt image engine for Termux — build stable-diffusion.cpp on-device? (slower one-time step)"})
		select {
		case <-m.approveImage:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		m.setImage(ComponentState{Status: StatusWorking, Message: "Building stable-diffusion.cpp from source on-device..."})
		buildDir, err := BuildFromSource(ctx, m.enginesRoot, "leejet/stable-diffusion.cpp", nil, m.onLog)
		if err != nil {
			m.setImage(ComponentState{Status: StatusError, Message: err.Error()})
			return "", err
		}
		bin, err := findSDBinary(buildDir)
		if err != nil {
			m.setImage(ComponentState{Status: StatusError, Message: err.Error()})
			return "", err
		}
		m.setImage(ComponentState{Status: StatusReady, BinaryPath: bin})
		return bin, nil
	}

	m.setImage(ComponentState{Status: StatusAwaitingApproval, Plan: &plan,
		Message: fmt.Sprintf("Download %s (%s, %s)?", plan.Component, plan.AssetName, humanBytes(plan.SizeBytes))})
	select {
	case <-m.approveImage:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	m.setImage(ComponentState{Status: StatusWorking, Plan: &plan, Message: "Downloading " + plan.AssetName})
	err = FetchAndExtract(m.enginesRoot, plan, "image", func(p download.Progress) {
		m.setImage(ComponentState{Status: StatusWorking, Plan: &plan, Progress: p, Message: "Downloading " + plan.AssetName})
	})
	if err != nil {
		m.setImage(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	bin, err := findSDBinary(filepath.Join(m.enginesRoot, PlatformKey(), "image"))
	if err != nil {
		m.setImage(ComponentState{Status: StatusError, Message: err.Error()})
		return "", err
	}
	m.setImage(ComponentState{Status: StatusReady, BinaryPath: bin})
	return bin, nil
}

func verifyRuns(bin string) (bool, error) {
	fi, err := os.Stat(bin)
	if err != nil {
		return false, err
	}
	if fi.Size() == 0 {
		return false, fmt.Errorf("binary is empty")
	}
	return true, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
