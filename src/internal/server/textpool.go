// TextPool replaces a single shared "active model" slot with a small pool
// of concurrently-running llama-server processes, keyed by model (+ vision
// projector). Two chats using the SAME model share one process — llama.cpp's
// own --parallel slots serve concurrent requests against it — and two chats
// using DIFFERENT models each get their own process, up to a hardware-sized
// concurrency cap. This is what makes "switch chats without interrupting
// the other one" actually true rather than just a UI illusion.
package server

import (
	"context"
	"sync"
	"time"

	"aistation/internal/engine"
	"aistation/internal/hw"
	"aistation/internal/registry"
	"aistation/internal/router"
)

// parallelSlotsPerModel: how many concurrent requests one llama-server
// instance is started with room to serve. Modest and fixed rather than
// hardware-scaled — each extra slot multiplies that model's KV-cache memory
// use, and our context sizes are already sized conservatively per model.
const parallelSlotsPerModel = 2

type poolEntry struct {
	proc     *engine.TextProcess
	refCount int
	lastUsed time.Time
}

type TextPool struct {
	mu            sync.Mutex
	entries       map[string]*poolEntry
	maxConcurrent int
}

func NewTextPool(profile hw.Profile) *TextPool {
	return &TextPool{entries: map[string]*poolEntry{}, maxConcurrent: maxConcurrentModels(profile)}
}

// maxConcurrentModels is a coarse, conservative tier based on how much
// headroom the hardware has — each concurrently-loaded model roughly costs
// its own file size in RAM/VRAM, so more headroom buys more simultaneous
// chats using different models before we start evicting idle ones.
func maxConcurrentModels(p hw.Profile) int {
	budget := p.BudgetBytes()
	switch {
	case budget == 0:
		return 1
	case budget < 8<<30:
		return 1
	case budget < 20<<30:
		return 2
	case budget < 40<<30:
		return 3
	default:
		return 4
	}
}

func poolKey(modelID, mmproj string) string { return modelID + "|" + mmproj }

// Acquire returns a running TextProcess for this model, starting one if
// needed (evicting the least-recently-used idle process if the pool is at
// capacity). The returned release func must be called when the caller is
// done using the process — it never stops the process itself, just marks it
// eligible for eviction again.
//
// started reports whether THIS call actually started a fresh process (true)
// vs. handed back an already-running one another caller may also be using
// (false). That distinction matters for OOM/failure retries: it's only safe
// to kill-and-restart-with-different-params a process you know you're the
// sole owner of — see Discard.
//
// The whole operation holds one lock, including the (multi-second) process
// start — this serializes cold starts of *different* models against each
// other, which is an acceptable, simple way to avoid a race where two
// concurrent first-requests for the same brand-new model each start their
// own process and leak one. Reusing an already-running model never waits
// on this beyond a quick map lookup.
func (p *TextPool) Acquire(binPath string, model registry.Model, params router.TextParams, mmproj string) (proc *engine.TextProcess, release func(), started bool, err error) {
	key := poolKey(model.ID, mmproj)

	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[key]; ok {
		e.refCount++
		e.lastUsed = time.Now()
		return e.proc, func() { p.release(key) }, false, nil
	}

	if len(p.entries) >= p.maxConcurrent {
		p.evictIdleLocked()
	}

	newProc, startErr := engine.StartTextServer(context.Background(), binPath, model.Path, params.ContextTokens, params.GPULayers, mmproj, parallelSlotsPerModel)
	if startErr != nil {
		return nil, nil, false, startErr
	}
	p.entries[key] = &poolEntry{proc: newProc, refCount: 1, lastUsed: time.Now()}
	return newProc, func() { p.release(key) }, true, nil
}

func (p *TextPool) release(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		e.refCount--
	}
}

// Discard force-stops and removes the pool entry for this model — only
// safe to call when the caller knows it's the sole owner (i.e. it just
// received started=true from Acquire and that same generation is now
// failing, e.g. from OOM at the params it was started with). Used to make
// a step-down retry actually take effect instead of silently reusing the
// same too-large process.
func (p *TextPool) Discard(model registry.Model, mmproj string) {
	key := poolKey(model.ID, mmproj)
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		e.proc.Stop()
		delete(p.entries, key)
	}
}

// evictIdleLocked drops the least-recently-used process with no active
// users, freeing a pool slot. If every running process is currently busy,
// it does nothing — Acquire just starts one more, temporarily exceeding the
// cap rather than blocking a user's request indefinitely.
func (p *TextPool) evictIdleLocked() {
	var victimKey string
	var oldest time.Time
	for k, e := range p.entries {
		if e.refCount > 0 {
			continue
		}
		if victimKey == "" || e.lastUsed.Before(oldest) {
			victimKey, oldest = k, e.lastUsed
		}
	}
	if victimKey != "" {
		p.entries[victimKey].proc.Stop()
		delete(p.entries, victimKey)
	}
}

// StopAll shuts down every running process — used on server shutdown.
func (p *TextPool) StopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.entries {
		e.proc.Stop()
		delete(p.entries, k)
	}
}
