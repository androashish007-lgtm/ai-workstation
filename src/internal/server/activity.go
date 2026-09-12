// Activity tracks what the workstation is currently doing, so the UI can
// show a tiny "working" / "possibly stuck" indicator instead of leaving the
// user staring at a spinner with no idea whether anything is actually
// happening. Multiple operations (e.g. two chats generating at once) are
// tracked independently, each keyed by its own opID — this is a UX signal,
// not a health check, so Snapshot() just summarizes: the longest-running
// operation's kind/elapsed time, a count, and whether ANY tracked op looks
// stuck.
package server

import (
	"fmt"
	"sync"
	"time"
)

type ActivityKind string

const (
	ActivityIdle                ActivityKind = "idle"
	ActivityGeneratingText      ActivityKind = "generating_text"
	ActivityGeneratingImage     ActivityKind = "generating_image"
	ActivityDownloadingModel    ActivityKind = "downloading_model"
	ActivityBootstrappingEngine ActivityKind = "bootstrapping_engine"
	ActivityLoadingModel        ActivityKind = "loading_model"
)

// stuckThresholds: how long without a heartbeat before we call it "possibly
// stuck." Image generation and on-device builds have no natural mid-point
// heartbeat, so they get a long threshold matched to their own expected
// worst case rather than a short universal one that would false-alarm on
// perfectly normal slow CPU inference.
var stuckThresholds = map[ActivityKind]time.Duration{
	ActivityGeneratingText:      45 * time.Second,
	ActivityGeneratingImage:     5 * time.Minute,
	ActivityDownloadingModel:    30 * time.Second,
	ActivityBootstrappingEngine: 60 * time.Second,
	ActivityLoadingModel:        90 * time.Second,
}

type opState struct {
	kind      ActivityKind
	detail    string // e.g. the specific model's display name, once known
	startedAt time.Time
	lastBeat  time.Time
}

type Activity struct {
	mu  sync.Mutex
	ops map[string]*opState
	seq uint64
}

func NewActivity() *Activity {
	return &Activity{ops: map[string]*opState{}}
}

// Begin starts tracking one independent operation and returns its ID (for
// Beat/SetDetail) and a function to call (typically via defer) when it
// finishes. Concurrent operations never interfere with each other's clocks.
// detail is optional extra context surfaced by ActiveDetails (typically the
// specific model's display name) — pass "" if it isn't known yet and set it
// later with SetDetail once it is (e.g. image generation only knows which
// model it picked after Begin, and may change models across a fallback
// retry within the same op).
func (a *Activity) Begin(kind ActivityKind, detail string) (string, func()) {
	a.mu.Lock()
	a.seq++
	id := fmt.Sprintf("op%d", a.seq)
	a.ops[id] = &opState{kind: kind, detail: detail, startedAt: time.Now(), lastBeat: time.Now()}
	a.mu.Unlock()
	return id, func() {
		a.mu.Lock()
		delete(a.ops, id)
		a.mu.Unlock()
	}
}

// SetDetail updates operation id's detail string (see Begin) — a no-op on a
// stale or already-ended id.
func (a *Activity) SetDetail(id, detail string) {
	a.mu.Lock()
	if o, ok := a.ops[id]; ok {
		o.detail = detail
	}
	a.mu.Unlock()
}

// Beat records progress on operation id (a streamed token, a download
// progress tick) so its staleness clock resets. Safe to call with a stale
// or already-ended id — it's just a no-op then.
func (a *Activity) Beat(id string) {
	a.mu.Lock()
	if o, ok := a.ops[id]; ok {
		o.lastBeat = time.Now()
	}
	a.mu.Unlock()
}

type ActivitySnapshot struct {
	Kind           ActivityKind `json:"kind"`
	ElapsedSeconds int          `json:"elapsed_seconds"`
	PossiblyStuck  bool         `json:"possibly_stuck"`
	Count          int          `json:"count"` // how many operations are running right now
}

// Snapshot summarizes all currently-running operations: the longest-running
// one's kind/elapsed time (most representative of "how long has the user
// been waiting"), how many are running, and whether any of them looks stuck.
func (a *Activity) Snapshot() ActivitySnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ops) == 0 {
		return ActivitySnapshot{Kind: ActivityIdle}
	}
	var longest *opState
	stuckAny := false
	for _, o := range a.ops {
		if longest == nil || o.startedAt.Before(longest.startedAt) {
			longest = o
		}
		if threshold, ok := stuckThresholds[o.kind]; ok && time.Since(o.lastBeat) > threshold {
			stuckAny = true
		}
	}
	return ActivitySnapshot{
		Kind:           longest.kind,
		ElapsedSeconds: int(time.Since(longest.startedAt).Seconds()),
		PossiblyStuck:  stuckAny,
		Count:          len(a.ops),
	}
}

// ActiveOp is one currently-running operation, for callers (like the active
// models widget) that need every op rather than Snapshot's single-longest
// summary — e.g. two chats generating images with two different models at
// once should both show up, not just whichever started first.
type ActiveOp struct {
	Kind           ActivityKind `json:"kind"`
	Detail         string       `json:"detail,omitempty"`
	ElapsedSeconds int          `json:"elapsed_seconds"`
}

// ActiveDetails returns every currently-running operation of the given kind.
func (a *Activity) ActiveDetails(kind ActivityKind) []ActiveOp {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []ActiveOp
	for _, o := range a.ops {
		if o.kind != kind {
			continue
		}
		out = append(out, ActiveOp{Kind: o.kind, Detail: o.detail, ElapsedSeconds: int(time.Since(o.startedAt).Seconds())})
	}
	return out
}
