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
	// ImageFamilyFlux2 is BFL's FLUX.2 line (klein/dev) — unlike every other
	// family here, stable-diffusion.cpp can't run it from one checkpoint
	// file: it needs the diffusion model, a separate VAE, and a separate
	// LLM text encoder passed as three distinct files (see Model.ImageRole
	// and PairedVAE/PairedTextEncoder below).
	ImageFamilyFlux2 ImageFamily = "flux2"
)

// ImageRole classifies an image-kind file as either a standalone checkpoint
// (the default, empty value — everything before FLUX.2) or one of the two
// component files a FLUX.2 diffusion model needs alongside it. Component
// files are never themselves selectable/generatable — they only exist to be
// paired onto a Flux2-family checkpoint (see linkFluxComponents).
type ImageRole string

const (
	ImageRoleCheckpoint  ImageRole = "" // a normal, standalone image model
	ImageRoleVAE         ImageRole = "vae"
	ImageRoleTextEncoder ImageRole = "text_encoder"
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
	// ImageRole/PairedVAE/PairedTextEncoder exist only for FLUX.2 support —
	// see ImageRole's doc comment. Both Paired* fields hold filenames (not
	// full paths, matching PairedProjector's convention) and are only ever
	// set on a Kind==KindImage, ImageFamily==ImageFamilyFlux2 checkpoint.
	ImageRole         ImageRole `json:"image_role,omitempty"`
	PairedVAE         string    `json:"paired_vae,omitempty"`
	PairedTextEncoder string    `json:"paired_text_encoder,omitempty"`
	DetectedAt        time.Time `json:"detected_at"`
	ParseError        string    `json:"parse_error,omitempty"`
}

// VisionCapable reports whether this text model has a paired multimodal
// projector registered alongside it.
func (m Model) VisionCapable() bool {
	return m.PairedProjector != ""
}

// IsImageComponent reports whether this is a FLUX.2 VAE/text-encoder file
// rather than a standalone, selectable image checkpoint.
func (m Model) IsImageComponent() bool {
	return m.ImageRole != ImageRoleCheckpoint
}

// FluxReady reports whether a FLUX.2 checkpoint has both files it needs to
// actually run alongside it. Always true for every other family/role, since
// only FLUX.2 checkpoints have this extra requirement.
func (m Model) FluxReady() bool {
	if m.ImageFamily != ImageFamilyFlux2 || m.IsImageComponent() {
		return true
	}
	return m.PairedVAE != "" && m.PairedTextEncoder != ""
}

type snapshot struct {
	Models map[string]Model `json:"models"` // key: absolute path
}

// Registry is the in-memory, mutex-protected, disk-backed model catalog.
type Registry struct {
	mu        sync.RWMutex
	models    map[string]Model // key: absolute path
	dataFile  string
	textDirs  []string          // scanned in order; index 0 is this app's own default models/text folder
	imageDirs []string          // same, for models/image
	watcher   *fsnotify.Watcher // set once Watch() is running; nil otherwise (SetDirs then only updates the dir lists, no live watch to extend)
	onChange  func()
}

// New builds a registry that scans textDirs/imageDirs — each normally a
// single-element slice (this app's own models/text or models/image folder)
// unless the user has added extra folders via SetDirs (e.g. a faster drive
// alongside the portable default).
func New(dataDir string, textDirs, imageDirs []string) *Registry {
	return &Registry{
		models:    map[string]Model{},
		dataFile:  filepath.Join(dataDir, "registry.json"),
		textDirs:  textDirs,
		imageDirs: imageDirs,
	}
}

// Dirs returns the current folders scanned for one kind, in scan order.
func (r *Registry) Dirs(kind ModelKind) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if kind == KindText {
		return append([]string{}, r.textDirs...)
	}
	return append([]string{}, r.imageDirs...)
}

// SetDirs replaces the full list of folders scanned for one kind and
// rescans immediately. Callers are expected to keep index 0 as this app's
// own default folder (SetDirs itself doesn't enforce that) — see
// handlers_api.go's model-dirs endpoint, which always does. Best-effort
// extends the live file watcher (if running) to the new folders; a folder
// removed from the list simply stops being scanned on the next rescan
// rather than having its stale watch explicitly torn down.
func (r *Registry) SetDirs(kind ModelKind, dirs []string) {
	r.mu.Lock()
	if kind == KindText {
		r.textDirs = dirs
	} else {
		r.imageDirs = dirs
	}
	w := r.watcher
	r.mu.Unlock()
	if w != nil {
		for _, d := range dirs {
			w.Add(d) // best-effort: already watched, or doesn't exist yet
		}
	}
	r.RescanAll()
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

// RescanAll walks every configured model directory (see Dirs/SetDirs),
// registers anything new or changed, and drops entries whose file no longer
// exists (whether because it was deleted, or because its folder was
// removed from the scanned list).
func (r *Registry) RescanAll() {
	r.mu.RLock()
	textDirs := append([]string{}, r.textDirs...)
	imageDirs := append([]string{}, r.imageDirs...)
	r.mu.RUnlock()

	seen := map[string]bool{}
	scan := func(dir string, isText bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
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
			r.registerIfChanged(abs, isText)
		}
	}
	for _, dir := range textDirs {
		scan(dir, true)
	}
	for _, dir := range imageDirs {
		scan(dir, false)
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
	r.linkFluxComponents()
	if changed {
		r.save()
		r.notify()
	}
}

// linkFluxComponents pairs the installed VAE and text-encoder files onto
// every installed FLUX.2 checkpoint. Unlike linkVisionProjectors, there's no
// filename relationship to match on (a diffusion model, its VAE, and its
// text encoder are published under completely unrelated names) — instead
// this assumes the common single-user case of at most one VAE and one
// text-encoder file installed at a time (they're shared across every
// klein/dev size per stable-diffusion.cpp's own docs) and pairs whichever
// one of each is found onto every Flux2-family checkpoint.
func (r *Registry) linkFluxComponents() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var vaeFile, teFile string
	for _, m := range r.models {
		if m.Kind != KindImage {
			continue
		}
		if m.ImageRole == ImageRoleVAE && vaeFile == "" {
			vaeFile = m.Filename
		}
		if m.ImageRole == ImageRoleTextEncoder && teFile == "" {
			teFile = m.Filename
		}
	}
	for path, m := range r.models {
		if m.Kind != KindImage || m.ImageFamily != ImageFamilyFlux2 || m.IsImageComponent() {
			continue
		}
		if m.PairedVAE != vaeFile || m.PairedTextEncoder != teFile {
			m.PairedVAE = vaeFile
			m.PairedTextEncoder = teFile
			r.models[path] = m
		}
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
		// Same size: assume unchanged, skip re-hashing multi-GB files — but
		// still cheaply re-derive filename-based classification (image
		// family/role) from scratch every scan, without re-reading the file.
		// Otherwise a model registered before a classifier change (e.g. the
		// FLUX.2 family/VAE/text-encoder detection added alongside this
		// comment) would keep its stale pre-upgrade classification forever,
		// since nothing else ever prompts a full re-fingerprint of a file
		// whose size hasn't changed.
		if !isText {
			refreshed := existing
			refreshed.ImageFamily = guessImageFamily(refreshed.Filename, refreshed.Architecture)
			refreshed.ImageRole = guessImageRole(refreshed.Filename)
			if refreshed.ImageFamily != existing.ImageFamily || refreshed.ImageRole != existing.ImageRole {
				r.mu.Lock()
				r.models[absPath] = refreshed
				r.mu.Unlock()
				r.save()
			}
		}
		return
	}

	// The same file may already be registered under a different absolute
	// path — this whole portable folder is designed to be moved between
	// drive letters/mount points (a USB stick moved from one computer's
	// D: to another's E:, an SD card reformatted and copied back), and a
	// changed prefix alone would otherwise look like "every model file is
	// new" and force re-hashing potentially tens of GB purely because the
	// path string changed, not the bytes. Filename+exact size is a cheap,
	// no-re-read signal that it's the same file relocated.
	if reused, ok := r.reuseByFilenameAndSize(filepath.Base(absPath), info.Size()); ok {
		reused.Path = absPath
		reused.DetectedAt = time.Now()
		r.mu.Lock()
		r.models[absPath] = reused
		r.mu.Unlock()
		r.save()
		r.notify()
		return
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

// reuseByFilenameAndSize looks for an already-registered model matching by
// filename and exact byte size, regardless of its stored absolute path —
// see registerIfChanged for why this specific pair is trusted as "same
// file" without re-reading it.
func (r *Registry) reuseByFilenameAndSize(filename string, size int64) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.models {
		if m.Filename == filename && m.SizeBytes == size {
			return m, true
		}
	}
	return Model{}, false
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
	} else {
		m.ImageRole = guessImageRole(m.Filename)
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
	// Checked before the plain "flux" case below since "flux-2"/"flux2"
	// always also contains "flux".
	case strings.Contains(f, "flux2") || strings.Contains(f, "flux-2") || strings.Contains(f, "flux.2") || strings.Contains(f, "flux_2"):
		return ImageFamilyFlux2
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

// guessImageRole flags a FLUX.2 VAE or text-encoder file by filename —
// there's no metadata to parse for a role like this (they're ordinary
// safetensors/GGUF weight files), but every source that publishes them
// (black-forest-labs, unsloth, city96, leejet's own GGUF repos) names them
// this way, matching stable-diffusion.cpp's own docs (docs/flux2.md).
func guessImageRole(filename string) ImageRole {
	f := strings.ToLower(filename)
	switch {
	case strings.Contains(f, "vae") || strings.Contains(f, "_ae.") || strings.Contains(f, "-ae."):
		return ImageRoleVAE
	case strings.Contains(f, "qwen") || strings.Contains(f, "mistral-small"):
		return ImageRoleTextEncoder
	}
	return ImageRoleCheckpoint
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

// Has reports whether a model with this ID and kind is currently installed
// — used to validate a user's explicit model pick before persisting it.
func (r *Registry) Has(kind ModelKind, id string) bool {
	for _, m := range r.Snapshot() {
		if m.Kind == kind && m.ID == id {
			return true
		}
	}
	return false
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
	r.mu.Lock()
	r.watcher = w
	dirs := append(append([]string{}, r.textDirs...), r.imageDirs...)
	r.mu.Unlock()
	for _, dir := range dirs {
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
