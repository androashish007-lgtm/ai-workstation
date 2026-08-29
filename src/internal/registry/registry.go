// Package registry watches the shared models/text and models/image folders,
// fingerprints and classifies whatever appears there, and persists the
// result to data/registry.json. Dropping a file in is the entire "install"
// step — there is no manual catalog to edit.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"aistation/internal/safego"
)

type ModelKind string

const (
	KindText  ModelKind = "text"  // chat / instruct GGUF
	KindImage ModelKind = "image" // T2I diffusion GGUF/safetensors
)

// ImageFamily is a best-effort hint used only to pick sane default
// resolution/step defaults; never surfaced to the user as a setting.
type ImageFamily string

const (
	ImageFamilyUnknown ImageFamily = ""
	ImageFamilySD15    ImageFamily = "sd1.5"
	ImageFamilySD2     ImageFamily = "sd2"
	ImageFamilySDXL    ImageFamily = "sdxl"
	ImageFamilyFlux    ImageFamily = "flux"
	ImageFamilySD3     ImageFamily = "sd3"
)

// Model is one registered file: a fingerprinted, classified entry the router
// can pick from without ever touching the filesystem itself.
type Model struct {
	ID                string      `json:"id"` // sha256, short form
	Path              string      `json:"path"`
	Filename          string      `json:"filename"`
	SHA256            string      `json:"sha256"`
	SizeBytes         int64       `json:"size_bytes"`
	Kind              ModelKind   `json:"kind"`
	Architecture      string      `json:"architecture,omitempty"`
	Name              string      `json:"name,omitempty"`
	SizeLabel         string      `json:"size_label,omitempty"`
	ContextLength     int         `json:"context_length,omitempty"`
	IsVisionProjector bool        `json:"is_vision_projector,omitempty"`
	PairedProjector   string      `json:"paired_projector,omitempty"` // filename of matched mmproj, if any
	ImageFamily       ImageFamily `json:"image_family,omitempty"`
	DetectedAt        time.Time   `json:"detected_at"`
	ParseError        string      `json:"parse_error,omitempty"`
}

// VisionCapable reports whether this text model has a paired multimodal
// projector registered alongside it.
func (m Model) VisionCapable() bool {
	return m.PairedProjector != ""
}

type snapshot struct {
	Models map[string]Model `json:"models"` // key: absolute path
}

// Registry is the in-memory, mutex-protected, disk-backed model catalog.
type Registry struct {
	mu       sync.RWMutex
	models   map[string]Model // key: absolute path
	dataFile string
	textDir  string
	imageDir string
	onChange func()
}

func New(dataDir, textDir, imageDir string) *Registry {
	return &Registry{
		models:   map[string]Model{},
		dataFile: filepath.Join(dataDir, "registry.json"),
		textDir:  textDir,
		imageDir: imageDir,
	}
}

// OnChange registers a callback fired (best-effort, non-blocking) whenever
// the registry contents change, so the router/UI can react without polling.
func (r *Registry) OnChange(fn func()) { r.onChange = fn }

func (r *Registry) notify() {
	if r.onChange != nil {
		safego.Go(r.onChange)
	}
}

// Load reads the persisted registry.json, if present, then re-scans both
// model folders so the persisted state self-heals from any offline edits
// (files added/removed/moved while the app wasn't running).
func (r *Registry) Load() error {
	if b, err := os.ReadFile(r.dataFile); err == nil {
		var snap snapshot
		if err := json.Unmarshal(b, &snap); err == nil {
			r.mu.Lock()
			r.models = snap.Models
			if r.models == nil {
				r.models = map[string]Model{}
			}
			r.mu.Unlock()
		}
	}
	r.RescanAll()
	return nil
}

func (r *Registry) save() {
	r.mu.RLock()
	snap := snapshot{Models: r.models}
	r.mu.RUnlock()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp := r.dataFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	os.Rename(tmp, r.dataFile)
}

// RescanAll walks both model directories, registers anything new or changed,
// and drops entries whose file no longer exists.
func (r *Registry) RescanAll() {
	seen := map[string]bool{}
	for _, dir := range []string{r.textDir, r.imageDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".part") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if info, err := e.Info(); err == nil && info.Size() == 0 {
				continue // placeholder or still-empty download target, never a real model
			}
			path := filepath.Join(dir, e.Name())
			abs, err := filepath.Abs(path)
			if err != nil {
				continue
			}
			seen[abs] = true
			r.registerIfChanged(abs, dir == r.textDir)
		}
	}
	r.mu.Lock()
	changed := false
	for path := range r.models {
		if !seen[path] {
			delete(r.models, path)
			changed = true
		}
	}
	r.mu.Unlock()
	r.linkVisionProjectors()
	if changed {
		r.save()
		r.notify()
	}
}

func (r *Registry) registerIfChanged(absPath string, isText bool) {
	info, err := os.Stat(absPath)
	if err != nil {
		return
	}
	r.mu.RLock()
	existing, ok := r.models[absPath]
	r.mu.RUnlock()
	if ok && existing.SizeBytes == info.Size() {
		return // cheap heuristic: same size, assume unchanged, skip re-hashing multi-GB files
	}
	m, err := fingerprintAndClassify(absPath, isText)
	if err != nil {
		log.Printf("registry: failed to register %s: %v", absPath, err)
		return
	}
	r.mu.Lock()
	r.models[absPath] = m
	r.mu.Unlock()
	r.save()
	r.notify()
}

func fingerprintAndClassify(absPath string, isText bool) (Model, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return Model{}, err
	}
	sum, err := sha256File(absPath)
	if err != nil {
		return Model{}, err
	}
	m := Model{
		ID:         sum[:16],
		Path:       absPath,
		Filename:   filepath.Base(absPath),
		SHA256:     sum,
		SizeBytes:  info.Size(),
		Kind:       KindImage,
		DetectedAt: time.Now(),
	}
	if isText {
		m.Kind = KindText
	}
	meta, err := ParseGGUFMeta(absPath)
	if err != nil {
		m.ParseError = err.Error()
		if !isText {
			m.ImageFamily = guessImageFamily(m.Filename, "")
		}
		return m, nil // still register it; capability detection just degrades
	}
	m.Architecture = meta.Architecture
	m.Name = meta.Name
	m.SizeLabel = meta.SizeLabel
	m.ContextLength = meta.ContextLength
	m.IsVisionProjector = meta.IsVisionClip
	if !isText {
		m.ImageFamily = guessImageFamily(m.Filename, meta.Architecture)
	}
	return m, nil
}

func guessImageFamily(filename, architecture string) ImageFamily {
	f := strings.ToLower(filename + " " + architecture)
	switch {
	case strings.Contains(f, "flux"):
		return ImageFamilyFlux
	case strings.Contains(f, "sdxl") || strings.Contains(f, "xl"):
		return ImageFamilySDXL
	case strings.Contains(f, "sd3"):
		return ImageFamilySD3
	case strings.Contains(f, "sd2") || strings.Contains(f, "2.1") || strings.Contains(f, "2-1"):
		return ImageFamilySD2
	case strings.Contains(f, "sd1") || strings.Contains(f, "1.5") || strings.Contains(f, "v1-5"):
		return ImageFamilySD15
	}
	return ImageFamilyUnknown
}

// linkVisionProjectors pairs mmproj-style projector files with the base text
// model that shares the longest filename prefix, so "vision capable" can be
// derived automatically instead of requiring the user to configure a pair.
func (r *Registry) linkVisionProjectors() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var projectors []Model
	for _, m := range r.models {
		if m.Kind == KindText && m.IsVisionProjector {
			projectors = append(projectors, m)
		}
	}
	if len(projectors) == 0 {
		return
	}
	for path, m := range r.models {
		if m.Kind != KindText || m.IsVisionProjector {
			continue
		}
		best, bestScore := "", 0
		for _, p := range projectors {
			score := commonPrefixLen(strings.ToLower(m.Filename), strings.ToLower(p.Filename))
			if score > bestScore {
				bestScore, best = score, p.Filename
			}
		}
		if bestScore >= 4 { // require a meaningful shared prefix, not a coincidence
			m.PairedProjector = best
			r.models[path] = m
		}
	}
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Snapshot returns a copy of all currently registered models.
func (r *Registry) Snapshot() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Model, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m)
	}
	return out
}

func (r *Registry) ByKind(kind ModelKind) []Model {
	var out []Model
	for _, m := range r.Snapshot() {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// Watch runs an fsnotify watcher on both model folders until ctx-like stop
// channel closes. Falls back silently to relying on RescanAll-on-demand if
// the watcher can't be created (e.g. exotic filesystem on some Android
// storage providers).
func (r *Registry) Watch(stop <-chan struct{}) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("registry: file watcher unavailable (%v); falling back to periodic scan", err)
		r.pollLoop(stop)
		return
	}
	defer w.Close()
	for _, dir := range []string{r.textDir, r.imageDir} {
		if err := w.Add(dir); err != nil {
			log.Printf("registry: could not watch %s: %v", dir, err)
		}
	}
	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false
	for {
		select {
		case <-stop:
			return
		case _, ok := <-w.Events:
			if !ok {
				return
			}
			if !pending {
				pending = true
				debounce.Reset(750 * time.Millisecond)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("registry: watcher error: %v", err)
		case <-debounce.C:
			pending = false
			r.RescanAll()
		}
	}
}

func (r *Registry) pollLoop(stop <-chan struct{}) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.RescanAll()
		}
	}
}
