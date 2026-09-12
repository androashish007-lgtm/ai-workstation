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
	"strings"
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
	// ready is non-nil and open while this entry's process is still
	// starting (a placeholder reserving the slot) — closed (and the entry
	// removed from the map, on failure) once the start finishes. Only ever
	// read/closed with p.mu held. Its purpose is letting a concurrent
	// Acquire for a DIFFERENT model proceed immediately instead of queuing
	// behind this one's cold start — see Acquire's doc comment for why the
	// old design serialized every cold start through one lock held for the
	// whole load. A waiter that wakes up to find the entry gone (the load
	// it was waiting on failed) simply falls through to starting its own
	// attempt, same as if nothing had ever been there.
	ready chan struct{}
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
//
// Thresholds were raised (previously 8/20/40GB) after a live incident on a
// 16GB-RAM machine: BudgetBytes() there was ~11GB (already just this
// machine's own single-model headroom, not a per-slot allowance), which sat
// in the old ">= 8GB" tier and let two ~5-7GB models load at once —
// pushing system RAM to ~90%+ and starving both. A second concurrent slot
// now requires roughly double a typical modern model's size in headroom, so
// two of them actually fit instead of merely being allowed to try.
func maxConcurrentModels(p hw.Profile) int {
	budget := p.BudgetBytes()
	switch {
	case budget == 0:
		return 1
	case budget < 16<<30:
		return 1
	case budget < 32<<30:
		return 2
	case budget < 48<<30:
		return 3
	default:
		return 4
	}
}

func poolKey(modelID, mmproj string) string { return modelID + "|" + mmproj }

// ResidentModel is one currently-loaded text model, for the active-models
// widget — Busy reports whether it's serving a request right now (refCount
// > 0) vs. just kept warm for reuse.
type ResidentModel struct {
	ModelID  string
	Busy     bool
	LastUsed time.Time
}

// Snapshot lists every currently-resident (loaded) model, unkeyed by which
// chat is using it — a model loaded once is shared across every chat that
// picks it, so "resident" is a global, not per-session, fact. A model still
// mid-cold-start (see Acquire) has no process yet and is left out — nothing
// meaningful to report until it either succeeds or fails.
func (p *TextPool) Snapshot() []ResidentModel {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ResidentModel, 0, len(p.entries))
	for k, e := range p.entries {
		if e.proc == nil {
			continue
		}
		modelID := k
		if i := strings.IndexByte(k, '|'); i >= 0 {
			modelID = k[:i]
		}
		out = append(out, ResidentModel{ModelID: modelID, Busy: e.refCount > 0, LastUsed: e.lastUsed})
	}
	return out
}

// HasResident reports whether this model is already loaded and immediately
// usable — for callers deciding whether an extra ancillary use (e.g. a quick
// classification aside) is basically free (an already-warm process, no
// wait) versus would trigger a full cold start (or a wait on someone else's
// in-progress one). Callers doing the latter should check this first and
// skip rather than risk that wait, since it's meant to be a cheap routing
// aid, not something worth blocking on.
func (p *TextPool) HasResident(modelID, mmproj string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[poolKey(modelID, mmproj)]
	return ok && e.proc != nil
}

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
// Cold starts of *different* models no longer serialize against each other:
// the pool lock is only held for the brief map lookup/reservation, never
// across the actual (multi-second-to-multi-minute, on slow storage) process
// start — a placeholder entry (proc == nil, ready open) reserves the slot so
// a second concurrent Acquire for the SAME brand-new model waits on that one
// specific load via its ready channel instead of starting a duplicate
// process, while an Acquire for any OTHER model proceeds immediately. This
// used to share one lock for the whole load, so switching to a chat needing
// a different, not-yet-loaded model could sit blocked for however long an
// unrelated chat's cold start took — the opposite of the "switching chats
// never interrupts another" this pool exists for.
//
// ctx is honored both for the actual process start (StartTextServer) and
// while waiting on someone else's in-progress load — this is what makes the
// Stop button actually interrupt a chat stuck cold-loading, rather than only
// cancelling the request that's already given up while the load silently
// continues in the background.
func (p *TextPool) Acquire(ctx context.Context, binPath string, model registry.Model, params router.TextParams, mmproj string) (proc *engine.TextProcess, release func(), started bool, err error) {
	key := poolKey(model.ID, mmproj)

	for {
		p.mu.Lock()
		if e, ok := p.entries[key]; ok {
			if e.ready != nil {
				ready := e.ready
				p.mu.Unlock()
				select {
				case <-ready:
					continue // re-check: it's now either ready or gone (failed)
				case <-ctx.Done():
					return nil, nil, false, ctx.Err()
				}
			}
			e.refCount++
			e.lastUsed = time.Now()
			p.mu.Unlock()
			return e.proc, func() { p.release(key) }, false, nil
		}

		if len(p.entries) >= p.maxConcurrent {
			p.evictIdleLocked()
		}
		ready := make(chan struct{})
		p.entries[key] = &poolEntry{ready: ready}
		p.mu.Unlock()

		newProc, startErr := engine.StartTextServer(ctx, binPath, model.Path, params.ContextTokens, params.GPULayers, mmproj, parallelSlotsPerModel)

		p.mu.Lock()
		if startErr != nil {
			delete(p.entries, key)
			close(ready)
			p.mu.Unlock()
			return nil, nil, false, startErr
		}
		p.entries[key] = &poolEntry{proc: newProc, refCount: 1, lastUsed: time.Now()}
		close(ready)
		p.mu.Unlock()
		return newProc, func() { p.release(key) }, true, nil
	}
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
	if e, ok := p.entries[key]; ok && e.proc != nil {
		e.proc.Stop()
		delete(p.entries, key)
	}
}

// evictIdleLocked drops the least-recently-used process with no active
// users, freeing a pool slot. If every running process is currently busy
// (or still mid-cold-start — never a valid eviction target), it does
// nothing — Acquire just starts one more, temporarily exceeding the cap
// rather than blocking a user's request indefinitely.
func (p *TextPool) evictIdleLocked() {
	var victimKey string
	var oldest time.Time
	for k, e := range p.entries {
		if e.proc == nil || e.refCount > 0 {
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
		if e.proc != nil {
			e.proc.Stop()
		}
		delete(p.entries, k)
	}
}
