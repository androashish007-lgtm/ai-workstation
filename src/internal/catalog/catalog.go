// Package catalog holds a small, curated, hardware-tier -> recommended-model
// manifest shipped with the app (data/model-catalog.json), and compares it
// against what's currently registered so the UI can suggest a download
// instead of requiring the user to go find models themselves. Nothing here
// ever downloads on its own — Suggest only returns candidates; the caller
// downloads only after explicit UI approval.
package catalog

import (
	"encoding/json"
	"os"
	"sort"

	"aistation/internal/hw"
	"aistation/internal/registry"
)

type Entry struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Kind         registry.ModelKind `json:"kind"`
	URL          string             `json:"url"`
	SHA256       string             `json:"sha256,omitempty"`
	SizeBytes    int64              `json:"size_bytes"`
	MinRAMBytes  uint64             `json:"min_ram_bytes"`
	MinVRAMBytes uint64             `json:"min_vram_bytes,omitempty"`
	Notes        string             `json:"notes,omitempty"`
	Tier         int                `json:"tier"`              // 1=smallest/fastest .. higher=larger/better quality
	Vision       bool               `json:"vision,omitempty"` // true for a vision-capable chat model or its paired encoder
	PairWith     string             `json:"pair_with,omitempty"` // catalog id of a companion entry required alongside this one (e.g. a vision encoder) — downloading either one fetches both
}

type Catalog struct {
	Entries []Entry `json:"entries"`
}

func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Suggest returns catalog entries of the requested kind that (a) fit the
// given hardware profile and (b) are not already installed (matched by
// checksum against the registry), best tier first. limit caps how many
// suggestions come back so the UI isn't flooded. When visionOnly is true,
// only vision-capable entries (or their paired encoder) are considered —
// use this when the request actually needs to look at an attached image,
// so the suggestion is one that will actually fix the problem.
func Suggest(c *Catalog, kind registry.ModelKind, profile hw.Profile, installed []registry.Model, limit int, visionOnly bool) []Entry {
	have := map[string]bool{}
	for _, m := range installed {
		if m.SHA256 != "" {
			have[m.SHA256] = true
		}
	}
	budget := profile.InstallBudgetBytes()
	var candidates []Entry
	for _, e := range c.Entries {
		if e.Kind != kind {
			continue
		}
		if visionOnly && !e.Vision {
			continue
		}
		if e.SHA256 != "" && have[e.SHA256] {
			continue
		}
		if e.MinRAMBytes > 0 && budget > 0 && e.MinRAMBytes > budget {
			continue
		}
		if e.MinVRAMBytes > 0 && profile.VRAMBytes > 0 && e.MinVRAMBytes > profile.VRAMBytes {
			continue
		}
		candidates = append(candidates, e)
	}
	// Stable so that among same-tier entries (e.g. a vision model and its
	// paired encoder), the one listed first in the catalog — conventionally
	// the main model, whose notes explain the pairing — wins ties and is
	// what BestFit's single pick surfaces.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Tier > candidates[j].Tier })
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

// BestFit returns the single best entry the hardware can run, or nil.
func BestFit(c *Catalog, kind registry.ModelKind, profile hw.Profile, installed []registry.Model, visionOnly bool) *Entry {
	s := Suggest(c, kind, profile, installed, 1, visionOnly)
	if len(s) == 0 {
		return nil
	}
	return &s[0]
}
