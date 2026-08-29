// On-device build fallback used only on Termux, where neither project
// publishes a prebuilt binary that's guaranteed to run (llama.cpp ships an
// android-arm64 asset that's tried first as a prebuilt shortcut; stable-
// diffusion.cpp ships none at all). `pkg install` here is scoped entirely to
// Termux's own $PREFIX userspace — it never touches the Android system
// partition and needs no root, so it doesn't violate "no system-wide
// install," it's simply how Termux obtains any userspace tool.
package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// EnsureTermuxToolchain installs clang/cmake/git/make via Termux's pkg
// manager if they aren't already on PATH. No-op (and safe to call) on any
// non-Termux platform.
func EnsureTermuxToolchain(onLog func(string)) error {
	if !IsTermux() {
		return nil
	}
	need := []string{}
	for _, tool := range []string{"clang", "cmake", "git", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			need = append(need, tool)
		}
	}
	if len(need) == 0 {
		return nil
	}
	onLog(fmt.Sprintf("Termux: installing build tools via pkg (user-space, Termux-only): %v", need))
	cmd := exec.Command("pkg", append([]string{"install", "-y"}, need...)...)
	cmd.Stdout, cmd.Stderr = logWriter(onLog), logWriter(onLog)
	return cmd.Run()
}

// BuildFromSource clones repo at a shallow depth, configures it with cmake,
// and builds it. Returns the build output directory to search for the
// resulting binary with FindBinary.
func BuildFromSource(ctx context.Context, enginesRoot, repo string, cmakeArgs []string, onLog func(string)) (string, error) {
	if err := EnsureTermuxToolchain(onLog); err != nil {
		return "", fmt.Errorf("installing termux build tools: %w", err)
	}
	name := filepath.Base(repo)
	srcDir := filepath.Join(enginesRoot, "_build", name)
	if _, err := os.Stat(filepath.Join(srcDir, ".git")); err != nil {
		onLog("Cloning " + repo + " (one-time, this can take a while)...")
		if err := os.MkdirAll(filepath.Dir(srcDir), 0o755); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "https://github.com/"+repo+".git", srcDir)
		cmd.Stdout, cmd.Stderr = logWriter(onLog), logWriter(onLog)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git clone: %w", err)
		}
	}
	buildDir := filepath.Join(srcDir, "build")
	onLog("Configuring build (cmake)...")
	cfgArgs := append([]string{"-B", buildDir, "-S", srcDir, "-DCMAKE_BUILD_TYPE=Release"}, cmakeArgs...)
	cfg := exec.CommandContext(ctx, "cmake", cfgArgs...)
	cfg.Stdout, cfg.Stderr = logWriter(onLog), logWriter(onLog)
	if err := cfg.Run(); err != nil {
		return "", fmt.Errorf("cmake configure: %w", err)
	}

	onLog("Compiling (this can take 10-30+ minutes on a phone CPU)...")
	jobs := fmt.Sprintf("%d", runtime.NumCPU())
	build := exec.CommandContext(ctx, "cmake", "--build", buildDir, "--config", "Release", "-j", jobs)
	build.Stdout, build.Stderr = logWriter(onLog), logWriter(onLog)
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("cmake build: %w", err)
	}
	return buildDir, nil
}

// logWriter adapts a line-callback into an io.Writer for subprocess
// stdout/stderr, so build progress streams to the terminal/UI live.
type callbackWriter struct{ fn func(string) }

func (w callbackWriter) Write(p []byte) (int, error) {
	if w.fn != nil {
		w.fn(string(p))
	}
	return len(p), nil
}

func logWriter(fn func(string)) callbackWriter { return callbackWriter{fn: fn} }
