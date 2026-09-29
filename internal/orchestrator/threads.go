package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	maxThreadTitle = 80
	// Matches the store's cap; checked here so an oversized body is refused, not truncated.
	maxTurnsBytes = 256 * 1024
	// untitledThread keeps the sidebar row clickable; the client renames it from the first prompt.
	untitledThread = "New conversation"
)

// registerThreadRoutes mounts the /v1/threads family. No store check here: the
// store opens in a background goroutine and is nil when Routes() runs, so the
// nil case is handled per request by projectStore.
func (s *Server) registerThreadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/threads", s.auth(s.handleThreadList))
	mux.HandleFunc("POST /v1/threads", s.auth(s.handleThreadCreate))
	mux.HandleFunc("GET /v1/threads/{id}", s.auth(s.handleThreadGet))
	mux.HandleFunc("PATCH /v1/threads/{id}", s.auth(s.handleThreadPatch))
	mux.HandleFunc("DELETE /v1/threads/{id}", s.auth(s.handleThreadDelete))
}

// thread loads the caller's thread by id, scoped to the authenticated org, or answers 404.
func (s *Server) thread(w http.ResponseWriter, r *http.Request, st *store.Store) *store.Thread {
	t, err := st.GetThread(r.Context(), fastOrg(r), r.PathValue("id"))
	if err != nil || t == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return nil
	}
	return t
}

func (s *Server) handleThreadList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	limit := httpx.QueryInt(r, "limit", 0) // absent, unparseable or not positive means no limit
	list, err := st.ListThreads(r.Context(), fastOrg(r), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": list})
}

func (s *Server) handleThreadCreate(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	var body struct {
		Title string `json:"title"`
		Turns string `json:"turns"`
	}
	// An empty body is a legitimate "start a new conversation".
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(body.Turns) > maxTurnsBytes {
		writeErr(w, http.StatusBadRequest, "transcript too large")
		return
	}
	org := fastOrg(r)
	now := time.Now().Unix()
	t := store.Thread{
		ID:        newThreadID(),
		OrgID:     org,
		Owner:     org,
		Title:     titleOr(body.Title, untitledThread),
		Turns:     body.Turns,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.SaveThread(r.Context(), t); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleThreadGet(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if t := s.thread(w, r, st); t != nil {
		writeJSON(w, http.StatusOK, t)
	}
}

// handleThreadPatch is the autosave path. Pointer fields distinguish an omitted key from an empty one.
func (s *Server) handleThreadPatch(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	t := s.thread(w, r, st)
	if t == nil {
		return
	}
	var body struct {
		Title *string `json:"title"`
		Turns *string `json:"turns"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Title != nil {
		t.Title = titleOr(*body.Title, t.Title)
	}
	if body.Turns != nil {
		if len(*body.Turns) > maxTurnsBytes {
			writeErr(w, http.StatusBadRequest, "transcript too large")
			return
		}
		t.Turns = *body.Turns
	}
	t.UpdatedAt = time.Now().Unix()
	if err := st.SaveThread(r.Context(), *t); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleThreadDelete reads first so another customer's id is a 404, not a 204
// that confirms the id exists.
func (s *Server) handleThreadDelete(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	t := s.thread(w, r, st)
	if t == nil {
		return
	}
	if err := st.DeleteThread(r.Context(), fastOrg(r), t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newThreadID() string { return fmt.Sprintf("th-%d-%s", time.Now().UnixMilli(), randHex4()) }

// titleOr trims and caps a title, falling back when nothing is left.
func titleOr(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if len(s) > maxThreadTitle {
		s = s[:maxThreadTitle]
	}
	return s
}
