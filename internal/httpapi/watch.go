package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// handleWatch streams file events for one directory as Server-Sent Events, one
// envd watch JSON line per event; the first is {"op":"ready"}. A stream lasts at
// most maxForegroundExecTimeout, then ends with an "end" event and the client
// reconnects, as SSE clients do. Not recursive.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// The stream starts on the ready line, so a watch that cannot start is an
	// ordinary HTTP error rather than a 200 that fails inside the stream.
	started := false
	err := s.d.WatchDir(r.Context(), id, dir, maxForegroundExecTimeout, func(line []byte) {
		if !started {
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
		}
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	})
	switch {
	case err == nil && started:
		fmt.Fprint(w, "event: end\ndata: {}\n\n")
		flusher.Flush()
	case err != nil && started:
		fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		flusher.Flush()
	case errors.Is(err, rt.ErrUnsupported):
		writeErrCode(w, http.StatusNotImplemented, "watch_unsupported", err.Error())
	case err != nil:
		writeErr(w, statusForErr(err), err.Error())
	default:
		writeErr(w, http.StatusServiceUnavailable, "the watch ended before it started")
	}
	s.auditDetail(r, "sandbox.fs.watch", id, map[string]any{"path": dir})
}
