package server

import (
	"net/http"
	"strconv"
)

// handleLogs backs the Settings > Logs tab: pass ?since=<cursor> (the
// previous response's own cursor) to fetch only new lines since last time,
// or omit it (or pass 0) to fetch everything currently buffered — the tab
// does the former on every poll after its first load, so an open Logs tab
// costs a tiny incremental fetch, not a full re-read of the buffer.
func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	var since uint64
	if s := r.URL.Query().Get("since"); s != "" {
		since, _ = strconv.ParseUint(s, 10, 64)
	}
	lines, cursor := a.logs.Since(since)
	writeJSON(w, map[string]any{"lines": lines, "cursor": cursor})
}
