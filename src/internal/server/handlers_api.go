package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aistation/internal/catalog"
	"aistation/internal/download"
	"aistation/internal/engine"
	"aistation/internal/registry"
	"aistation/internal/safego"
	"aistation/internal/session"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (a *App) handleHW(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.profile)
}

type systemUsageResponse struct {
	CPUPercent    float64          `json:"cpu_percent"`
	RAMPercent    float64          `json:"ram_percent"`
	RAMUsedBytes  uint64           `json:"ram_used_bytes"`
	RAMTotalBytes uint64           `json:"ram_total_bytes"`
	CPUAvailable  bool             `json:"cpu_available"`
	Activity      ActivitySnapshot `json:"activity"`
}

// handleSystemUsage backs the small always-on CPU/RAM + activity indicator.
// Both readings come from in-memory state (a background sampler for
// CPU/RAM, the activity tracker for the rest), so this never blocks on I/O.
// Engine bootstrap (download/build) isn't pushed into the shared activity
// tracker — it has its own longer-lived, approval-gated lifecycle — so it's
// derived here as a fallback only when nothing else is active, straight
// from the engine manager's own state.
func (a *App) handleSystemUsage(w http.ResponseWriter, r *http.Request) {
	u := a.usage.Latest()
	act := a.activity.Snapshot()
	if act.Kind == ActivityIdle {
		if derived, ok := a.deriveBootstrapActivity(); ok {
			act = derived
		}
	}
	writeJSON(w, systemUsageResponse{
		CPUPercent:    u.CPUPercent,
		RAMPercent:    u.RAMPercent,
		RAMUsedBytes:  u.RAMUsedBytes,
		RAMTotalBytes: u.RAMTotalBytes,
		CPUAvailable:  u.CPUAvailable,
		Activity:      act,
	})
}

func (a *App) deriveBootstrapActivity() (ActivitySnapshot, bool) {
	text, image := a.engines.Snapshot()
	for _, c := range []engine.ComponentState{text, image} {
		if c.Status == engine.StatusWorking && !c.WorkingSince.IsZero() {
			elapsed := time.Since(c.WorkingSince)
			return ActivitySnapshot{
				Kind:           ActivityBootstrappingEngine,
				ElapsedSeconds: int(elapsed.Seconds()),
				PossiblyStuck:  elapsed > stuckThresholds[ActivityBootstrappingEngine],
			}, true
		}
	}
	for _, c := range []engine.ComponentState{text, image} {
		if c.Status == engine.StatusAwaitingApproval {
			return ActivitySnapshot{Kind: "awaiting_approval"}, true
		}
	}
	return ActivitySnapshot{}, false
}

func (a *App) handleRegistry(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.reg.Snapshot())
}

func (a *App) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	kind := registry.KindText
	if r.URL.Query().Get("kind") == "image" {
		kind = registry.KindImage
	}
	installed := a.reg.Snapshot()
	suggestions := catalog.Suggest(a.cat, kind, a.profile, installed, 3)
	writeJSON(w, suggestions)
}

type bootstrapResponse struct {
	HW               any                  `json:"hw"`
	Models           []registry.Model     `json:"models"`
	SuggestionsText  []catalog.Entry      `json:"suggestions_text"`
	SuggestionsImage []catalog.Entry      `json:"suggestions_image"`
	TextEngine       any                  `json:"text_engine"`
	ImageEngine      any                  `json:"image_engine"`
	Sessions         []SessionSummaryView `json:"sessions"`
	ActiveSessionID  string               `json:"active_session_id"`
}

// SessionSummaryView adds a live "is this chat currently generating"
// flag — sourced from the GenerationManager, not persisted state — so the
// sidebar can show which background chats are still working.
type SessionSummaryView struct {
	session.Summary
	Generating bool `json:"generating"`
}

func (a *App) withGeneratingFlag(list []session.Summary) []SessionSummaryView {
	active := a.generations.ActiveSessions()
	out := make([]SessionSummaryView, len(list))
	for i, s := range list {
		out[i] = SessionSummaryView{Summary: s, Generating: active[s.ID]}
	}
	return out
}

func (a *App) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	models := a.reg.Snapshot()
	textState, imageState := a.engines.Snapshot()
	sessions, _ := a.sessions.List()
	cfg := a.config.Load()

	activeID := cfg.ActiveSessionID
	if activeID == "" || !sessionExists(sessions, activeID) {
		s := a.sessions.New()
		a.sessions.Save(s)
		activeID = s.ID
		cfg.ActiveSessionID = activeID
		a.config.Save(cfg)
		sessions, _ = a.sessions.List()
	}

	writeJSON(w, bootstrapResponse{
		HW:               a.profile,
		Models:           models,
		SuggestionsText:  catalog.Suggest(a.cat, registry.KindText, a.profile, models, 3),
		SuggestionsImage: catalog.Suggest(a.cat, registry.KindImage, a.profile, models, 3),
		TextEngine:       textState,
		ImageEngine:      imageState,
		Sessions:         a.withGeneratingFlag(sessions),
		ActiveSessionID:  activeID,
	})
}

func sessionExists(list []session.Summary, id string) bool {
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

func (a *App) handleEngineStatus(w http.ResponseWriter, r *http.Request) {
	text, image := a.engines.Snapshot()
	writeJSON(w, map[string]any{"text": text, "image": image})
}

func (a *App) handleEngineApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Component string `json:"component"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch body.Component {
	case "text":
		a.engines.ApproveText()
	case "image":
		a.engines.ApproveImage()
	default:
		http.Error(w, "component must be 'text' or 'image'", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) handleDownloadModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CatalogID string `json:"catalog_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var entry *catalog.Entry
	for i := range a.cat.Entries {
		if a.cat.Entries[i].ID == body.CatalogID {
			entry = &a.cat.Entries[i]
			break
		}
	}
	if entry == nil {
		http.Error(w, "unknown catalog_id", http.StatusNotFound)
		return
	}

	a.dlMu.Lock()
	if a.dlProgress.Active {
		a.dlMu.Unlock()
		http.Error(w, "a download is already in progress", http.StatusConflict)
		return
	}
	a.dlProgress = DownloadStatus{Active: true, EntryID: entry.ID, Name: entry.Name, TotalBytes: entry.SizeBytes}
	a.dlMu.Unlock()

	destDir := a.dirs.ModelsText
	if entry.Kind == registry.KindImage {
		destDir = a.dirs.ModelsImage
	}
	filename := filenameFromURL(entry.URL)
	dest := filepath.Join(destDir, filename)

	safego.Go(func() {
		opID, end := a.activity.Begin(ActivityDownloadingModel)
		defer end()
		_, err := download.Fetch(download.Options{
			URL:            entry.URL,
			Dest:           dest,
			ExpectedSHA256: entry.SHA256,
			OnProgress: func(p download.Progress) {
				a.dlMu.Lock()
				a.dlProgress.DoneBytes = p.DoneBytes
				if p.TotalBytes > 0 {
					a.dlProgress.TotalBytes = p.TotalBytes
				}
				a.dlMu.Unlock()
				a.activity.Beat(opID)
			},
		})
		a.dlMu.Lock()
		a.dlProgress.Active = false
		a.dlProgress.Done = true
		if err != nil {
			a.dlProgress.Error = err.Error()
		}
		a.dlMu.Unlock()
		if err == nil {
			a.reg.RescanAll()
		}
	})

	w.WriteHeader(http.StatusAccepted)
}

func filenameFromURL(u string) string {
	u = strings.SplitN(u, "?", 2)[0]
	return filepath.Base(u)
}

func (a *App) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	a.dlMu.Lock()
	defer a.dlMu.Unlock()
	writeJSON(w, a.dlProgress)
}

func (a *App) handleListSessions(w http.ResponseWriter, r *http.Request) {
	list, err := a.sessions.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, a.withGeneratingFlag(list))
}

func (a *App) handleNewSession(w http.ResponseWriter, r *http.Request) {
	s := a.sessions.New()
	if err := a.sessions.Save(s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg := a.config.Load()
	cfg.ActiveSessionID = s.ID
	a.config.Save(cfg)
	writeJSON(w, s)
}

func (a *App) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, err := a.sessions.Load(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cfg := a.config.Load()
	cfg.ActiveSessionID = id
	a.config.Save(cfg)
	writeJSON(w, s)
}

func (a *App) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.ContainsAny(id, "/\\.") {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	os.Remove(filepath.Join(a.dirs.Sessions, id+".json"))
	os.RemoveAll(filepath.Join(a.dirs.Images, id))
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleImage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session")
	file := r.PathValue("file")
	if strings.ContainsAny(sessionID, "/\\.") || strings.ContainsAny(file, "/\\") || !strings.HasSuffix(file, ".png") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	path := filepath.Join(a.dirs.Images, sessionID, file)
	http.ServeFile(w, r, path)
}

// handleLANURL reports the actual http://<lan-ip>:<port> other devices on
// the network should use — computed from the port this server actually
// bound (not whatever host the browser happens to be viewing it from, which
// is 127.0.0.1 when the desktop app opened it locally and useless for a
// phone to scan).
func (a *App) handleLANURL(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	port := a.port
	a.mu.Unlock()
	url := LANURL(port)
	if url == "" {
		http.Error(w, "no LAN address available", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"url": url})
}

// handleModelUsage backs the "Model usage" tab: which installed models are
// actually used, most to least, sourced from the on-disk usage log.
func (a *App) handleModelUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.usageLog.Snapshot())
}
