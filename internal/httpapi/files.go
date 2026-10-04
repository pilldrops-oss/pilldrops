package httpapi

import (
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// safeFilePath rejects any ".." segment so a path cannot escape the sandbox
// root; the process runtime is not isolated. The query value is already URL-decoded.
func safeFilePath(p string) bool {
	return !slices.Contains(strings.Split(p, "/"), "..")
}

// confinedPathMsg is what a caller can act on when the guest refuses a path.
// envd confines every file path to the sandbox home and answers one
// deliberately generic "invalid path" for everything outside it, so an escape
// attempt and a typo cannot be told apart from outside — that generic answer is
// kept, and only the rule is added to it.
const confinedPathMsg = "invalid path: file paths are taken relative to the sandbox home, " +
	"and one that resolves outside it is refused"

// writeFSErr answers a failed file operation with what went wrong with the
// caller's path: missing 404, already there 409, not allowed 403, unusable or
// outside the sandbox home 400. Only a failure it cannot attribute to the path
// is the node's 503. Read, stat and list used to answer 404 for everything and
// the mutations 503, so a mkdir over a file told the caller to retry.
func writeFSErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rt.ErrPathRefused):
		// The caller's path, not this node's health: a 503 sent them away with
		// "retry", which a path the guest will refuse every time never satisfies.
		writeErr(w, http.StatusBadRequest, confinedPathMsg)
	case errors.Is(err, rt.ErrPathNotFound), errors.Is(err, iofs.ErrNotExist):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, iofs.ErrExist):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, iofs.ErrPermission):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, iofs.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, statusForErr(err), err.Error())
	}
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	id, path := r.PathValue("id"), r.URL.Query().Get("path")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	if !safeFilePath(path) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	fs, err := s.d.FS(id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	// /files is exempt from the general request cap (bodyCapExempt) and bounded
	// by its own PD_FILES_MAX_BYTES instead, so the limit that fires is named.
	body := http.MaxBytesReader(w, r.Body, s.filesMaxBytes)
	var n int64
	if sfs, ok := fs.(rt.StreamFS); ok {
		n, err = sfs.WriteFrom(r.Context(), path, body)
	} else {
		var data []byte
		if data, err = io.ReadAll(body); err == nil {
			n = int64(len(data))
			err = fs.Write(r.Context(), path, data)
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "request body too large") {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds the per-file limit PD_FILES_MAX_BYTES (%d bytes)", s.filesMaxBytes))
			return
		}
		writeFSErr(w, err)
		return
	}
	s.auditDetail(r, "sandbox.fs.write", id, map[string]any{"path": path, "size": n})
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "bytes": n})
}

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	id, path := r.PathValue("id"), r.URL.Query().Get("path")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if !safeFilePath(path) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	fs, err := s.d.FS(id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	if sfs, ok := fs.(rt.StreamFS); ok {
		// Stat first so a missing file is a 404 and not a half-written 200.
		if _, err := fs.Stat(r.Context(), path); err != nil {
			writeFSErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if err := sfs.ReadTo(r.Context(), path, w); err != nil {
			slog.Warn("file stream cut", "component", "httpapi", "sandbox", id, "path", path, "err", err)
		}
		return
	}
	data, err := fs.Read(r.Context(), path)
	if err != nil {
		writeFSErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (s *Server) handleListDir(w http.ResponseWriter, r *http.Request) {
	id, path := r.PathValue("id"), r.URL.Query().Get("path")
	if path == "" {
		path = "."
	}
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if !safeFilePath(path) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	fs, err := s.d.FS(id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	entries, err := fs.List(r.Context(), path)
	if err != nil {
		writeFSErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request)  { s.fsMutate(w, r, "mkdir") }
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request)   { s.fsMutate(w, r, "move") }
func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) { s.fsMutate(w, r, "remove") }

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	id, path := r.PathValue("id"), r.URL.Query().Get("path")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if !safeFilePath(path) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	fs, err := s.d.FS(id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	info, err := fs.Stat(r.Context(), path)
	if err != nil {
		writeFSErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) fsMutate(w http.ResponseWriter, r *http.Request, op string) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Path string `json:"path"`
		Src  string `json:"src"`
		Dst  string `json:"dst"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !safeFilePath(body.Path) || !safeFilePath(body.Src) || !safeFilePath(body.Dst) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	fs, err := s.d.FS(id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	switch op {
	case "mkdir":
		err = fs.Mkdir(r.Context(), body.Path)
	case "move":
		err = fs.Move(r.Context(), body.Src, body.Dst)
	case "remove":
		err = fs.Remove(r.Context(), body.Path)
	}
	if err != nil {
		writeFSErr(w, err)
		return
	}
	detail := map[string]any{"path": body.Path}
	if op == "move" {
		detail = map[string]any{"src": body.Src, "dst": body.Dst}
	}
	s.auditDetail(r, "sandbox.fs."+op, id, detail)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
