// Bootstrap resolves and fetches the llama.cpp and stable-diffusion.cpp
// engine binaries for the current platform. Release asset names roll with
// every build (llama.cpp tags are "b12345", stable-diffusion.cpp tags embed
// a commit hash), so instead of pinning an exact filename we resolve the
// most recent release from GitHub's API at bootstrap time and pick the asset
// whose name matches this platform/backend by substring — the naming scheme
// itself is stable even though exact tags aren't.
package engine

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"aistation/internal/download"
	"aistation/internal/hw"
)

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

func fetchReleases(repo string) ([]ghRelease, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repo+"/releases?per_page=5", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aistation/1.0")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases API returned %d for %s", resp.StatusCode, repo)
	}
	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}
	return releases, nil
}

// PlannedDownload describes what bootstrap intends to fetch, surfaced to the
// terminal/UI approval prompt before anything is fetched.
type PlannedDownload struct {
	Component string // "text engine (llama.cpp)" / "image engine (stable-diffusion.cpp)"
	AssetName string
	URL       string
	SizeBytes int64
	Extra     *PlannedDownload // e.g. matching CUDA runtime DLLs; unused now that we default to Vulkan/CPU
}

// resolveAsset finds the newest release (scanning a few recent ones, since
// the very latest can lack a matching asset) whose asset name contains every
// string in mustContain and none of excludeAny.
func resolveAsset(repo string, mustContain []string, excludeAny []string) (PlannedDownload, error) {
	releases, err := fetchReleases(repo)
	if err != nil {
		return PlannedDownload{}, err
	}
	for _, rel := range releases {
		for _, a := range rel.Assets {
			lower := strings.ToLower(a.Name)
			ok := true
			for _, m := range mustContain {
				if !strings.Contains(lower, strings.ToLower(m)) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			for _, x := range excludeAny {
				if strings.Contains(lower, strings.ToLower(x)) {
					ok = false
					break
				}
			}
			if ok {
				return PlannedDownload{AssetName: a.Name, URL: a.BrowserDownloadURL, SizeBytes: a.Size}, nil
			}
		}
	}
	return PlannedDownload{}, fmt.Errorf("no matching release asset found in %s for %v (excluding %v)", repo, mustContain, excludeAny)
}

// PlanLlamaCpp figures out which llama.cpp release asset fits this host.
func PlanLlamaCpp(profile hw.Profile) (PlannedDownload, error) {
	backend := PreferredBackend(profile)
	var must []string
	switch {
	case IsTermux():
		must = []string{"bin-android-arm64"}
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		if backend == BackendVulkan {
			must = []string{"bin-win-vulkan-x64"}
		} else {
			must = []string{"bin-win-cpu-x64"}
		}
	case runtime.GOOS == "windows" && runtime.GOARCH == "arm64":
		must = []string{"bin-win-cpu-arm64"}
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		must = []string{"bin-macos-arm64"}
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		must = []string{"bin-macos-x64"}
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		if backend == BackendVulkan {
			must = []string{"bin-ubuntu-vulkan-x64"}
		} else {
			must = []string{"bin-ubuntu-x64"}
		}
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		must = []string{"bin-ubuntu-arm64"}
	default:
		return PlannedDownload{}, fmt.Errorf("unsupported platform for prebuilt llama.cpp: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	p, err := resolveAsset("ggml-org/llama.cpp", must, []string{"xcframework", "-ui.", "cudart"})
	if err != nil {
		return p, err
	}
	p.Component = "text engine (llama.cpp)"
	return p, nil
}

// PlanStableDiffusionCpp figures out which stable-diffusion.cpp release asset
// fits this host. Returns ok=false (not an error) for Termux, where no
// prebuilt asset exists and the caller should fall back to BuildSDCppFromSource.
func PlanStableDiffusionCpp(profile hw.Profile) (PlannedDownload, bool, error) {
	if IsTermux() {
		return PlannedDownload{}, false, nil
	}
	backend := PreferredBackend(profile)
	var must []string
	switch {
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		if backend == BackendVulkan {
			must = []string{"bin-win-vulkan-x64"}
		} else {
			must = []string{"bin-win-cpu-x64"}
		}
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		must = []string{"bin-darwin-macos", "arm64"} // Metal is built into the default macOS build
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		must = []string{"bin-darwin-macos", "x64"}
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		if backend == BackendVulkan {
			must = []string{"bin-linux-ubuntu", "x86_64", "vulkan"}
		} else {
			must = []string{"bin-linux-ubuntu", "x86_64"}
		}
	default:
		return PlannedDownload{}, false, fmt.Errorf("unsupported platform for prebuilt stable-diffusion.cpp: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exclude := []string{"rocm", "sycl", "cudart"}
	if backend != BackendVulkan {
		exclude = append(exclude, "vulkan")
	}
	p, err := resolveAsset("leejet/stable-diffusion.cpp", must, exclude)
	if err != nil {
		return p, true, err
	}
	p.Component = "image engine (stable-diffusion.cpp)"
	return p, true, nil
}

// FetchAndExtract downloads a planned asset into engines/<platformKey>/<subdir>/
// and extracts it in place (zip on Windows assets, tar.gz elsewhere).
func FetchAndExtract(enginesRoot string, plan PlannedDownload, subdir string, onProgress func(download.Progress)) error {
	destDir := filepath.Join(enginesRoot, PlatformKey(), subdir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	archivePath := filepath.Join(enginesRoot, "_downloads", plan.AssetName)
	res, err := download.Fetch(download.Options{
		URL:        plan.URL,
		Dest:       archivePath,
		OnProgress: onProgress,
	})
	if err != nil {
		return err
	}
	_ = res
	defer os.Remove(archivePath)

	if strings.HasSuffix(plan.AssetName, ".zip") {
		return extractZip(archivePath, destDir)
	}
	if strings.HasSuffix(plan.AssetName, ".tar.gz") || strings.HasSuffix(plan.AssetName, ".tgz") {
		return extractTarGz(archivePath, destDir)
	}
	return fmt.Errorf("don't know how to extract %s", plan.AssetName)
}

func extractZip(archivePath, destDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if err := extractZipEntry(f, destDir); err != nil {
			return err
		}
	}
	return nil
}

func extractZipEntry(f *zip.File, destDir string) error {
	path := filepath.Join(destDir, filepath.Clean(f.Name))
	if !strings.HasPrefix(path, filepath.Clean(destDir)+string(os.PathSeparator)) {
		return fmt.Errorf("illegal zip entry path: %s", f.Name)
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(path, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()|0o755&f.Mode()|0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(destDir, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(path, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal tar entry path: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode)
			out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}

// FindBinary locates the named executable (with the right extension for this
// OS) anywhere under dir, since release archives vary in their internal
// folder layout.
func FindBinary(dir, name string) (string, error) {
	want := name
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	var found string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && strings.EqualFold(d.Name(), want) {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("%s not found under %s", want, dir)
	}
	return found, nil
}
