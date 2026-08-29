// Generation decouples an in-progress chat/image response from the HTTP
// request that started it, so switching sessions (or closing the tab) never
// interrupts it: POST /api/chat only kicks the work off in the background;
// GET /api/sessions/{id}/stream is a separate, reattachable SSE subscription
// that replays whatever's already happened and then follows live — connect,
// disconnect, reconnect from another tab, none of it touches the underlying
// generation.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type genEvent map[string]any

// Generation is one in-flight response for one session. Every event it
// produces is buffered (so a client that connects late, or reconnects,
// gets caught up) and fanned out to any currently-attached subscribers.
type Generation struct {
	SessionID string

	mu     sync.Mutex
	events []genEvent
	subs   map[chan genEvent]bool
	done   bool
}

func newGeneration(sessionID string) *Generation {
	return &Generation{SessionID: sessionID, subs: map[chan genEvent]bool{}}
}

func (g *Generation) emit(evt genEvent) {
	g.mu.Lock()
	g.events = append(g.events, evt)
	if evt["type"] == "done" || evt["type"] == "error" {
		g.done = true
	}
	for ch := range g.subs {
		select {
		case ch <- evt:
		default: // slow subscriber misses a live tick; it's still in the buffer for the next replay
		}
	}
	g.mu.Unlock()
}

// send matches the old sseWriter's call shape so the generation pipeline
// code reads the same as before.
func (g *Generation) send(evt map[string]any) { g.emit(evt) }

// subscribe attaches a new listener and returns everything buffered so far
// plus a channel for what happens next, and an unsubscribe func.
func (g *Generation) subscribe() ([]genEvent, chan genEvent, func()) {
	g.mu.Lock()
	replay := append([]genEvent{}, g.events...)
	ch := make(chan genEvent, 32)
	if !g.done {
		g.subs[ch] = true
	}
	g.mu.Unlock()
	return replay, ch, func() {
		g.mu.Lock()
		if _, ok := g.subs[ch]; ok {
			delete(g.subs, ch)
			close(ch)
		}
		g.mu.Unlock()
	}
}

// GenerationManager tracks at most one active Generation per session.
type GenerationManager struct {
	mu        sync.Mutex
	bySession map[string]*Generation
}

func NewGenerationManager() *GenerationManager {
	return &GenerationManager{bySession: map[string]*Generation{}}
}

// Start registers a new Generation for sessionID, or returns ok=false if one
// is already running there (callers should reject the new request rather
// than clobber an in-progress response).
func (m *GenerationManager) Start(sessionID string) (*Generation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.bySession[sessionID]; busy {
		return nil, false
	}
	g := newGeneration(sessionID)
	m.bySession[sessionID] = g
	return g, true
}

func (m *GenerationManager) Get(sessionID string) (*Generation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.bySession[sessionID]
	return g, ok
}

func (m *GenerationManager) Finish(sessionID string, g *Generation) {
	m.mu.Lock()
	if m.bySession[sessionID] == g {
		delete(m.bySession, sessionID)
	}
	m.mu.Unlock()
}

// ActiveSessions returns the set of session IDs with a generation currently
// running, so the sidebar can show a "still working" indicator on chats the
// user isn't looking at.
func (m *GenerationManager) ActiveSessions() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.bySession))
	for id := range m.bySession {
		out[id] = true
	}
	return out
}

// handleSessionStream is the reattachable SSE endpoint: it replays whatever
// a session's active generation has already produced, then streams new
// events live until done. If nothing is running for this session, it just
// reports idle and returns immediately — there's nothing to watch.
func (a *App) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	writeEvt := func(evt genEvent) {
		b, _ := json.Marshal(evt)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	gen, active := a.generations.Get(id)
	if !active {
		writeEvt(genEvent{"type": "idle"})
		return
	}

	replay, ch, unsub := gen.subscribe()
	defer unsub()
	for _, evt := range replay {
		writeEvt(evt)
		if evt["type"] == "done" || evt["type"] == "error" {
			return
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			writeEvt(evt)
			if evt["type"] == "done" || evt["type"] == "error" {
				return
			}
		}
	}
}
