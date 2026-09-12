// Package logbuf keeps a bounded, in-memory ring of the app's own recent
// log lines so the UI's Logs tab can tail them live without reading back
// from disk — the file on disk exists for after-the-fact debugging, not as
// the source the running app reads from.
package logbuf

import (
	"strings"
	"sync"
)

// Buffer is an io.Writer (plug it into log.SetOutput via io.MultiWriter
// alongside stderr/a file) that also serves recent lines back out via
// Since. Safe for concurrent use.
type Buffer struct {
	mu    sync.Mutex
	lines []string
	seq   uint64 // total lines ever written; the newest line's sequence number
	max   int
}

func New(max int) *Buffer {
	if max <= 0 {
		max = 1
	}
	return &Buffer{max: max}
}

// Write treats each call as one already-formatted log line (true for the
// standard library's log.Logger, which calls Write once per entry) —
// trailing newlines are trimmed since the frontend renders one line per
// entry itself.
func (b *Buffer) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	b.mu.Lock()
	b.seq++
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	b.mu.Unlock()
	return len(p), nil
}

// Since returns every line written after cursor `after` (0 means "from the
// oldest still buffered"), and the cursor to pass next time. If lines
// older than what's still buffered were requested, this silently starts
// from the oldest available rather than erroring — a client that was slow
// to poll just sees a gap instead of a failure.
func (b *Buffer) Since(after uint64) (lines []string, cursor uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldestKept := b.seq - uint64(len(b.lines))
	if after < oldestKept {
		after = oldestKept
	}
	if after >= b.seq {
		return nil, b.seq
	}
	start := int(after - oldestKept)
	return append([]string{}, b.lines[start:]...), b.seq
}
