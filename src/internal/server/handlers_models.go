package server

import (
	"fmt"
	"net/http"
	"os"
	"sort"
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
