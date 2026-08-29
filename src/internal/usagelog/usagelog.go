// Package usagelog records how often each installed model is actually used,
// so the UI can show a "most/least used" view instead of the user having to
// guess. Persisted as one small JSON file — this is a counter, not an
// analytics pipeline.
package usagelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Record struct {
	ModelID    string    `json:"model_id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"` // "text" or "image"
	Count      int       `json:"count"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type Log struct {
	mu      sync.Mutex
	path    string
	records map[string]*Record
}

func New(dataDir string) *Log {
	l := &Log{path: filepath.Join(dataDir, "model-usage.json"), records: map[string]*Record{}}
	l.load()
	return l
}

func (l *Log) load() {
	b, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var list []Record
	if err := json.Unmarshal(b, &list); err != nil {
		return
	}
	for i := range list {
		r := list[i]
		l.records[r.ModelID] = &r
	}
}

func (l *Log) save() {
	list := make([]Record, 0, len(l.records))
	for _, r := range l.records {
		list = append(list, *r)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	os.Rename(tmp, l.path)
}

// Record logs one use of modelID. Safe to call from any goroutine.
func (l *Log) Record(modelID, name, kind string) {
	if modelID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.records[modelID]
	if !ok {
		r = &Record{ModelID: modelID, Name: name, Kind: kind}
		l.records[modelID] = r
	}
	r.Name = name // keep the display name fresh in case it changed
	r.Count++
	r.LastUsedAt = time.Now()
	l.save()
}

// Snapshot returns all records, most-used first.
func (l *Log) Snapshot() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, len(l.records))
	for _, r := range l.records {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}
