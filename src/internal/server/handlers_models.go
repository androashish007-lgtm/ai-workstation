package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"aistation/internal/registry"
	"aistation/internal/router"
	"aistation/internal/usagelog"
)

// modelUsageEntry is one row on the "Model usage" tab: everything needed to
// decide whether to keep or delete an installed model in one place — what
// it's actually good for, and how much it's actually been used.
type modelUsageEntry struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Filename   string     `json:"filename"`
	Kind       string     `json:"kind"`
	SizeBytes  int64      `json:"size_bytes"`
	UseCase    string     `json:"use_case"`
	Count      int        `json:"count"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"` // nil means never used — a zero time.Time doesn't omitempty away on its own
	ParseError string     `json:"parse_error,omitempty"`
}

// bestUseCase describes in one short sentence what an installed model is
// actually good for, so the model-usage tab can help decide what to keep
// without the user having to already know what a "vision projector" or
// "SDXL" is.
func bestUseCase(m registry.Model) string {
	if m.Kind == registry.KindText {
		if m.IsVisionProjector {
			return "Vision encoder — pairs with a chat model, does nothing installed alone."
		}
		if m.VisionCapable() {
			return "Vision-capable chat — understands attached images as well as text."
		}
		switch {
		case m.SizeBytes < 2<<30: // <2GB
			return "Fast, lightweight chat — quick replies on modest hardware, weaker reasoning."
		case m.SizeBytes < 6<<30: // 2-6GB
			return "General-purpose chat — balanced speed and quality for everyday requests."
		default:
			return "Complex, high-quality chat — best reasoning and detail, slower to respond."
		}
	}
	// image
	if router.LooksLikeEditingModel(m.Filename) {
		return "Image editing — needs a reference image (img2img/inpaint/pix2pix-style), not plain text-to-image."
	}
	switch m.ImageFamily {
	case registry.ImageFamilyFlux:
		return "Highest-quality image generation (Flux), 1024×1024."
	case registry.ImageFamilySDXL, registry.ImageFamilySD3:
		return "High-detail image generation (SDXL), 1024×1024."
	case registry.ImageFamilySD2:
		return "Standard image generation, 768×768."
	default:
		return "Standard image generation (SD1.5-class), 512×512."
	}
}

// handleModelUsage backs the "Model usage" tab: every installed model —
// not just ones that happen to have a usage record — with its best use
// case and actual usage tally, grouped by folder (text/image), so the user
// can decide what's worth keeping.
func (a *App) handleModelUsage(w http.ResponseWriter, r *http.Request) {
	byID := map[string]usagelog.Record{}
	for _, rec := range a.usageLog.Snapshot() {
		byID[rec.ModelID] = rec
	}
	text, image := []modelUsageEntry{}, []modelUsageEntry{}
	for _, m := range a.reg.Snapshot() {
		rec, used := byID[m.ID]
		entry := modelUsageEntry{
			ID:         m.ID,
			Name:       displayModelName(m),
			Filename:   m.Filename,
			Kind:       string(m.Kind),
			SizeBytes:  m.SizeBytes,
			UseCase:    bestUseCase(m),
			ParseError: m.ParseError,
		}
		if used {
			entry.Count = rec.Count
			lastUsed := rec.LastUsedAt
			entry.LastUsedAt = &lastUsed
		}
		if m.Kind == registry.KindText {
			text = append(text, entry)
		} else {
			image = append(image, entry)
		}
	}
	sortUsage := func(list []modelUsageEntry) {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Count != list[j].Count {
				return list[i].Count > list[j].Count
			}
			return list[i].Name < list[j].Name
		})
	}
	sortUsage(text)
	sortUsage(image)
	writeJSON(w, map[string]any{"text": text, "image": image})
}

// activeModelEntry is one row in the active-models widget: a specific
// model that's either actually resident/generating right now, or (when
// nothing is) the most recent one used, so the widget never just shows
// blank the instant a chat finishes.
type activeModelEntry struct {
	Name       string     `json:"name"`
	Busy       bool       `json:"busy"`   // actively generating right now
	Loaded     bool       `json:"loaded"` // resident in memory (text only — image has no persistent residency)
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// handleActiveModels backs the "which model is currently in use" widget
// next to the CPU/RAM status badge. Text models can sit loaded-but-idle
// between requests (see TextPool) — those still show up as "loaded", just
// not "busy" — while image generation has no residency at all (sd-cli exits
// after every request, see engine/image.go), so an image model only ever
// appears here while a generation is actually in flight; once idle, the
// fallback below shows the last one used instead of leaving it empty.
func (a *App) handleActiveModels(w http.ResponseWriter, r *http.Request) {
	byID := map[string]registry.Model{}
	for _, m := range a.reg.Snapshot() {
		byID[m.ID] = m
	}

	var textEntries []activeModelEntry
	for _, res := range a.textPool.Snapshot() {
		m, ok := byID[res.ModelID]
		name := res.ModelID
		if ok {
			name = displayModelName(m)
		}
		lastUsed := res.LastUsed
		textEntries = append(textEntries, activeModelEntry{Name: name, Busy: res.Busy, Loaded: true, LastUsedAt: &lastUsed})
	}
	if len(textEntries) == 0 {
		if e, ok := a.lastUsageEntry(registry.KindText); ok {
			textEntries = append(textEntries, e)
		}
	}

	var imageEntries []activeModelEntry
	for _, op := range a.activity.ActiveDetails(ActivityGeneratingImage) {
		if op.Detail == "" {
			continue // model not selected yet (still resolving/waiting on the engine)
		}
		imageEntries = append(imageEntries, activeModelEntry{Name: op.Detail, Busy: true})
	}
	if len(imageEntries) == 0 {
		if e, ok := a.lastUsageEntry(registry.KindImage); ok {
			imageEntries = append(imageEntries, e)
		}
	}

	writeJSON(w, map[string]any{"text": textEntries, "image": imageEntries})
}

// lastUsageEntry reports the most-recently-used model of this kind from the
// usage log, as a not-currently-active fallback entry.
func (a *App) lastUsageEntry(kind registry.ModelKind) (activeModelEntry, bool) {
	var best *usagelog.Record
	for _, rec := range a.usageLog.Snapshot() {
		if rec.Kind != string(kind) {
			continue
		}
		r := rec
		if best == nil || r.LastUsedAt.After(best.LastUsedAt) {
			best = &r
		}
	}
	if best == nil {
		return activeModelEntry{}, false
	}
	lastUsed := best.LastUsedAt
	return activeModelEntry{Name: best.Name, LastUsedAt: &lastUsed}, true
}

// modelDirsKindView is one kind's (text/image) folder configuration: the
// fixed default folder inside this app's own directory (always scanned,
// never removable) and any extra folders added on top of it.
type modelDirsKindView struct {
	Default string   `json:"default"`
	Extra   []string `json:"extra"`
}

// handleGetModelDirs backs the "model folders" settings panel — lists
// where each kind currently looks for installed models, so the UI can show
// what's configured and let the user add/remove extra folders (e.g. one on
// a faster internal drive, alongside the portable default).
func (a *App) handleGetModelDirs(w http.ResponseWriter, r *http.Request) {
	cfg := a.config.Load()
	writeJSON(w, map[string]modelDirsKindView{
		"text":  {Default: a.dirs.ModelsText, Extra: cfg.ExtraModelDirsText},
		"image": {Default: a.dirs.ModelsImage, Extra: cfg.ExtraModelDirsImage},
	})
}

// handleSetModelDirs replaces the extra (non-default) folders scanned for
// one kind. This only points the app at folders the user has already
// populated themselves (e.g. by moving files there in a file manager) —
// it never copies or moves any model file on its own.
func (a *App) handleSetModelDirs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string   `json:"kind"`
		Dirs []string `json:"dirs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var kind registry.ModelKind
	var defaultDir string
	switch body.Kind {
	case "text":
		kind, defaultDir = registry.KindText, a.dirs.ModelsText
	case "image":
		kind, defaultDir = registry.KindImage, a.dirs.ModelsImage
	default:
		http.Error(w, "kind must be \"text\" or \"image\"", http.StatusBadRequest)
		return
	}

	var clean []string
	seen := map[string]bool{defaultDir: true} // the default is implicit, never listed as "extra"
	for _, d := range body.Dirs {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		info, err := os.Stat(d)
		if err != nil {
			http.Error(w, fmt.Sprintf("folder not found: %s", d), http.StatusBadRequest)
			return
		}
		if !info.IsDir() {
			http.Error(w, fmt.Sprintf("not a folder: %s", d), http.StatusBadRequest)
			return
		}
		seen[d] = true
		clean = append(clean, d)
	}

	cfg := a.config.Load()
	if kind == registry.KindText {
		cfg.ExtraModelDirsText = clean
	} else {
		cfg.ExtraModelDirsImage = clean
	}
	if err := a.config.Save(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.reg.SetDirs(kind, append([]string{defaultDir}, clean...))
	writeJSON(w, modelDirsKindView{Default: defaultDir, Extra: clean})
}

// handleDeleteModel removes an installed model file from disk by its
// registry ID — the "Model usage" tab's per-model delete button. The
// registry watcher would eventually notice the file is gone on its own,
// but rescanning immediately here means the UI reflects the deletion
// without waiting on the debounce.
func (a *App) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var target *registry.Model
	for _, m := range a.reg.Snapshot() {
		if m.ID == id {
			mm := m
			target = &mm
			break
		}
	}
	if target == nil {
		http.Error(w, "model not found", http.StatusNotFound)
		return
	}
	if err := os.Remove(target.Path); err != nil {
		http.Error(w, fmt.Sprintf("could not delete %s: %v (it may be in use — try again once any generation using it finishes)", target.Filename, err), http.StatusConflict)
		return
	}
	a.reg.RescanAll()
	w.WriteHeader(http.StatusNoContent)
}
