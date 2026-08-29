// Package server wires every other package into one local HTTP server: the
// unified chat+image API, the bundled UI, and the small set of endpoints the
// UI needs to show engine/model status and request approvals. This is the
// only package that knows about HTTP.
package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"aistation/internal/catalog"
	"aistation/internal/engine"
	"aistation/internal/hw"
	"aistation/internal/imageperf"
	"aistation/internal/project"
	"aistation/internal/registry"
	"aistation/internal/session"
	"aistation/internal/usagelog"
)

type Dirs struct {
	Root        string
	ModelsText  string
	ModelsImage string
	Engines     string
	Data        string
	Sessions    string
	Images      string
	Downloads   string
}

type App struct {
	dirs        Dirs
	reg         *registry.Registry
	cat         *catalog.Catalog
	engines     *engine.Manager
	sessions    *session.Manager
	config      *session.ConfigStore
	profile     hw.Profile
	usage       *hw.UsageSampler
	activity    *Activity
	generations *GenerationManager
	textPool    *TextPool
	usageLog    *usagelog.Log
	imagePerf   *imageperf.Store
	projects    *project.Store

	mu                    sync.Mutex
	textBinPath           string
	imageBinPath          string
	textBootstrapStarted  bool
	imageBootstrapStarted bool
	port                  int

	dlMu       sync.Mutex
	dlProgress DownloadStatus
}

type DownloadStatus struct {
	Active     bool   `json:"active"`
	EntryID    string `json:"entry_id,omitempty"`
	Name       string `json:"name,omitempty"`
	DoneBytes  int64  `json:"done_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	Error      string `json:"error,omitempty"`
	Done       bool   `json:"done"`
}

func NewApp(root string) (*App, error) {
	dirs := Dirs{
		Root:        root,
		ModelsText:  filepath.Join(root, "models", "text"),
		ModelsImage: filepath.Join(root, "models", "image"),
		Engines:     filepath.Join(root, "engines"),
		Data:        filepath.Join(root, "data"),
		Sessions:    filepath.Join(root, "data", "sessions"),
		Images:      filepath.Join(root, "data", "sessions", "images"),
		Downloads:   filepath.Join(root, "data", "downloads"),
	}

	profile := hw.Detect()
	log.Printf("hardware: %s/%s, %d cores, %.1fGB RAM, GPU=%s (%s)",
		profile.OS, profile.Arch, profile.CPUCores,
		float64(profile.TotalRAMBytes)/(1<<30), profile.GPUVendor, profile.GPUName)

	reg := registry.New(dirs.Data, dirs.ModelsText, dirs.ModelsImage)
	if err := reg.Load(); err != nil {
		return nil, fmt.Errorf("loading model registry: %w", err)
	}

	cat, err := catalog.Load(filepath.Join(dirs.Data, "model-catalog.json"))
	if err != nil {
		log.Printf("warning: model catalog not loaded (%v); download suggestions will be empty", err)
		cat = &catalog.Catalog{}
	}

	mgr := engine.NewManager(dirs.Engines, profile)
	sessions := session.NewManager(dirs.Sessions)
	config := session.NewConfigStore(dirs.Data)
	activity := NewActivity()

	return &App{
		dirs:        dirs,
		reg:         reg,
		cat:         cat,
		engines:     mgr,
		sessions:    sessions,
		config:      config,
		profile:     profile,
		usage:       hw.NewUsageSampler(),
		activity:    activity,
		generations: NewGenerationManager(),
		textPool:    NewTextPool(profile),
		usageLog:    usagelog.New(dirs.Data),
		imagePerf:   imageperf.New(dirs.Data),
		projects:    project.New(dirs.Data),
	}, nil
}

func (a *App) Registry() *registry.Registry { return a.reg }
func (a *App) Engines() *engine.Manager     { return a.engines }
func (a *App) Profile() hw.Profile          { return a.profile }

// StartWatcher runs the model-folder watcher until stop is closed.
func (a *App) StartWatcher(stop <-chan struct{}) {
	a.reg.Watch(stop)
}

// StartUsageSampler runs the CPU/RAM sampling loop until stop is closed.
func (a *App) StartUsageSampler(stop <-chan struct{}) {
	a.usage.Start(stop)
}

// Serve starts the HTTP server on both localhost and (if lan is true) every
// interface, returning the port actually bound (0 means "pick any free
// port").
func (a *App) Serve(ctx context.Context, port int, lan bool) (int, error) {
	mux := http.NewServeMux()
	a.registerRoutes(mux)

	host := "127.0.0.1"
	if lan {
		host = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return 0, err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	a.mu.Lock()
	a.port = actualPort
	a.mu.Unlock()

	srv := &http.Server{Handler: withRecover(mux)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		a.textPool.StopAll()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
		}
	}()
	return actualPort, nil
}

func withRecover(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic handling %s %s: %v", r.Method, r.URL.Path, rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// noCache forces revalidation on every request for the bundled UI so a
// browser can never keep running JS/CSS from before an update — this app
// gets replaced by overwriting bin/, not versioned URLs, so caching would
// otherwise silently mask a fix that was actually shipped.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		h.ServeHTTP(w, r)
	})
}

func (a *App) registerRoutes(mux *http.ServeMux) {
	mux.Handle("/", noCache(http.FileServerFS(webRoot())))
	mux.HandleFunc("GET /api/bootstrap", a.handleBootstrap)
	mux.HandleFunc("GET /api/hw", a.handleHW)
	mux.HandleFunc("GET /api/registry", a.handleRegistry)
	mux.HandleFunc("GET /api/suggestions", a.handleSuggestions)
	mux.HandleFunc("POST /api/downloads/model", a.handleDownloadModel)
	mux.HandleFunc("GET /api/downloads/status", a.handleDownloadStatus)
	mux.HandleFunc("GET /api/engine/status", a.handleEngineStatus)
	mux.HandleFunc("POST /api/engine/approve", a.handleEngineApprove)
	mux.HandleFunc("GET /api/sessions", a.handleListSessions)
	mux.HandleFunc("POST /api/sessions", a.handleNewSession)
	mux.HandleFunc("DELETE /api/sessions", a.handleDeleteAllSessions)
	mux.HandleFunc("GET /api/sessions/{id}", a.handleGetSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", a.handleDeleteSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", a.handlePatchSession)
	mux.HandleFunc("GET /api/sessions/{id}/stream", a.handleSessionStream)
	mux.HandleFunc("POST /api/sessions/{id}/stop", a.handleStopGeneration)
	mux.HandleFunc("GET /api/projects", a.handleListProjects)
	mux.HandleFunc("POST /api/projects", a.handleCreateProject)
	mux.HandleFunc("PUT /api/projects/{id}", a.handleUpdateProject)
	mux.HandleFunc("DELETE /api/projects/{id}", a.handleDeleteProject)
	mux.HandleFunc("POST /api/chat", a.handleChat)
	mux.HandleFunc("GET /api/qr", a.handleQR)
	mux.HandleFunc("GET /api/lan-url", a.handleLANURL)
	mux.HandleFunc("GET /api/system/usage", a.handleSystemUsage)
	mux.HandleFunc("GET /api/usage/models", a.handleModelUsage)
	mux.HandleFunc("GET /images/{session}/{file}", a.handleImage)
}
