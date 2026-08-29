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
	"aistation/internal/project"
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
	GPUPercent    float64          `json:"gpu_percent"`
	GPUAvailable  bool             `json:"gpu_available"`
	GPUVendor     string           `json:"gpu_vendor"`
	GPUName       string           `json:"gpu_name"`
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
		GPUPercent:    u.GPUPercent,
		GPUAvailable:  u.GPUAvailable,
		GPUVendor:     string(a.profile.GPUVendor),
		GPUName:       a.profile.GPUName,
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
	suggestions := catalog.Suggest(a.cat, kind, a.profile, installed, 3, false)
	writeJSON(w, suggestions)
}

// handleNoticeStatus backs a session.Message's Notice field: a persisted
// stand-in for a live event the UI might have missed always shows its
// explanation text, but the actual action (a Download or Approve button)
// needs to reflect the CURRENT state, not whatever was true when the
// message was saved — the user may well have already fixed it since. This
// re-derives the right suggestion/approval-status live, or reports
// "resolved" so the frontend can just not show a button at all.
func (a *App) handleNoticeStatus(w http.ResponseWriter, r *http.Request) {
	notice := r.URL.Query().Get("notice")
	installed := a.reg.Snapshot()

	hasModel := func(kind registry.ModelKind, vision bool) bool {
		for _, m := range installed {
			if m.Kind != kind || m.IsVisionProjector {
				continue
			}
			if vision && !m.VisionCapable() {
				continue
			}
			return true
		}
		return false
	}

	switch notice {
	case "no_text_model":
		if hasModel(registry.KindText, false) {
			writeJSON(w, map[string]any{"resolved": true})
			return
		}
		writeJSON(w, map[string]any{"kind": "text", "suggestion": catalog.BestFit(a.cat, registry.KindText, a.profile, installed, false)})
	case "no_vision_model":
		if hasModel(registry.KindText, true) {
			writeJSON(w, map[string]any{"resolved": true})
			return
		}
		writeJSON(w, map[string]any{"kind": "text", "suggestion": catalog.BestFit(a.cat, registry.KindText, a.profile, installed, true)})
	case "no_image_model":
		if hasModel(registry.KindImage, false) {
			writeJSON(w, map[string]any{"resolved": true})
			return
		}
		writeJSON(w, map[string]any{"kind": "image", "suggestion": catalog.BestFit(a.cat, registry.KindImage, a.profile, installed, false)})
	case "text_engine_approval":
		t, _ := a.engines.Snapshot()
		if t.Status == engine.StatusReady {
			writeJSON(w, map[string]any{"resolved": true})
			return
		}
		writeJSON(w, map[string]any{"component": "text", "engine_status": t})
	case "image_engine_approval":
		_, i := a.engines.Snapshot()
		if i.Status == engine.StatusReady {
			writeJSON(w, map[string]any{"resolved": true})
			return
		}
		writeJSON(w, map[string]any{"component": "image", "engine_status": i})
	default:
		http.Error(w, "unknown notice", http.StatusBadRequest)
	}
}

type bootstrapResponse struct {
	HW               any                  `json:"hw"`
	Models           []registry.Model     `json:"models"`
	SuggestionsText  []catalog.Entry      `json:"suggestions_text"`
	SuggestionsImage []catalog.Entry      `json:"suggestions_image"`
	TextEngine       any                  `json:"text_engine"`
	ImageEngine      any                  `json:"image_engine"`
	Sessions         []SessionSummaryView `json:"sessions"`
	Projects         []project.Project    `json:"projects"`
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
		s := a.sessions.New("")
		a.sessions.Save(s)
		activeID = s.ID
		cfg.ActiveSessionID = activeID
		a.config.Save(cfg)
		sessions, _ = a.sessions.List()
	}

	writeJSON(w, bootstrapResponse{
		HW:               a.profile,
		Models:           models,
		SuggestionsText:  catalog.Suggest(a.cat, registry.KindText, a.profile, models, 3, false),
		SuggestionsImage: catalog.Suggest(a.cat, registry.KindImage, a.profile, models, 3, false),
		TextEngine:       textState,
		ImageEngine:      imageState,
		Sessions:         a.withGeneratingFlag(sessions),
		Projects:         a.projects.List(),
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

	// A companion file (e.g. a vision model's required encoder) is
	// downloaded right after, in the same background task — otherwise a
	// suggestion card that only offers one of the two ends up installing a
	// model that's useless alone, with nothing telling the user a second
	// file was ever needed.
	var pair *catalog.Entry
	if entry.PairWith != "" && !a.modelInstalled(entry.PairWith) {
		for i := range a.cat.Entries {
			if a.cat.Entries[i].ID == entry.PairWith {
				pair = &a.cat.Entries[i]
				break
			}
		}
	}

	safego.Go(func() {
		opID, end := a.activity.Begin(ActivityDownloadingModel)
		defer end()
		if err := a.fetchCatalogEntry(opID, *entry); err != nil {
			a.dlMu.Lock()
			a.dlProgress.Active = false
			a.dlProgress.Done = true
			a.dlProgress.Error = err.Error()
			a.dlMu.Unlock()
			return
		}
		if pair != nil {
			a.dlMu.Lock()
			a.dlProgress = DownloadStatus{Active: true, EntryID: pair.ID, Name: pair.Name, TotalBytes: pair.SizeBytes}
			a.dlMu.Unlock()
			if err := a.fetchCatalogEntry(opID, *pair); err != nil {
				a.dlMu.Lock()
				a.dlProgress.Active = false
				a.dlProgress.Done = true
				a.dlProgress.Error = "companion file failed: " + err.Error()
				a.dlMu.Unlock()
				a.reg.RescanAll()
				return
			}
		}
		a.dlMu.Lock()
		a.dlProgress.Active = false
		a.dlProgress.Done = true
		a.dlMu.Unlock()
		a.reg.RescanAll()
	})

	w.WriteHeader(http.StatusAccepted)
}

// modelInstalled reports whether the catalog entry with the given id is
// already present in the registry (matched by checksum).
func (a *App) modelInstalled(catalogID string) bool {
	var target *catalog.Entry
	for i := range a.cat.Entries {
		if a.cat.Entries[i].ID == catalogID {
			target = &a.cat.Entries[i]
			break
		}
	}
	if target == nil || target.SHA256 == "" {
		return false
	}
	for _, m := range a.reg.Snapshot() {
		if m.SHA256 == target.SHA256 {
			return true
		}
	}
	return false
}

// fetchCatalogEntry downloads one catalog entry to its destination folder,
// reporting progress on the shared a.dlProgress used by the polling UI.
func (a *App) fetchCatalogEntry(opID string, entry catalog.Entry) error {
	destDir := a.dirs.ModelsText
	if entry.Kind == registry.KindImage {
		destDir = a.dirs.ModelsImage
	}
	dest := filepath.Join(destDir, filenameFromURL(entry.URL))
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
	return err
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
	var body struct {
		ProjectID string `json:"project_id"`
	}
	json.NewDecoder(r.Body).Decode(&body) // optional body; empty/absent is fine
	if body.ProjectID != "" {
		if _, ok := a.projects.Get(body.ProjectID); !ok {
			http.Error(w, "unknown project_id", http.StatusNotFound)
			return
		}
	}
	s := a.sessions.New(body.ProjectID)
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
	if _, active := a.generations.Get(id); active {
		http.Error(w, "this chat is still generating a response — wait for it to finish before deleting", http.StatusConflict)
		return
	}
	a.deleteSessionFiles(id)
	a.clearActiveIfDeleted(id)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteAllSessions deletes every chat except any currently
// generating a response (those are skipped, not force-stopped) and reports
// how many of each.
func (a *App) handleDeleteAllSessions(w http.ResponseWriter, r *http.Request) {
	list, err := a.sessions.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	active := a.generations.ActiveSessions()
	deleted, skipped := 0, 0
	for _, s := range list {
		if active[s.ID] {
			skipped++
			continue
		}
		a.deleteSessionFiles(s.ID)
		a.clearActiveIfDeleted(s.ID)
		deleted++
	}
	writeJSON(w, map[string]any{"deleted": deleted, "skipped": skipped})
}

func (a *App) deleteSessionFiles(id string) {
	os.Remove(filepath.Join(a.dirs.Sessions, id+".json"))
	os.RemoveAll(filepath.Join(a.dirs.Images, id))
}

func (a *App) clearActiveIfDeleted(id string) {
	cfg := a.config.Load()
	if cfg.ActiveSessionID == id {
		cfg.ActiveSessionID = ""
		a.config.Save(cfg)
	}
}

// handlePatchSession currently supports moving a chat into/out of a
// project ({"project_id": "..."} or {"project_id": ""} to ungroup).
func (a *App) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		ProjectID *string `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s, err := a.sessions.Load(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if body.ProjectID != nil {
		if *body.ProjectID != "" {
			if _, ok := a.projects.Get(*body.ProjectID); !ok {
				http.Error(w, "unknown project_id", http.StatusNotFound)
				return
			}
		}
		s.ProjectID = *body.ProjectID
	}
	if err := a.sessions.Save(s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s)
}

func (a *App) handleListProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.projects.List())
}

func (a *App) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	p := a.projects.Create(strings.TrimSpace(body.Name), body.Notes)
	writeJSON(w, p)
}

func (a *App) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name  string `json:"name"`
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	p, ok := a.projects.Update(id, strings.TrimSpace(body.Name), body.Notes)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, p)
}

// handleDeleteProject deletes the project but never the chats inside it —
// they're ungrouped back to the top level instead, since a "delete this
// grouping" action shouldn't silently take conversations with it.
func (a *App) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.projects.Delete(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	list, _ := a.sessions.List()
	for _, summary := range list {
		if summary.ProjectID != id {
			continue
		}
		s, err := a.sessions.Load(summary.ID)
		if err != nil {
			continue
		}
		s.ProjectID = ""
		a.sessions.Save(s)
	}
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

