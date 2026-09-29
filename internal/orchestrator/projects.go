package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Projects are the durable thing a user works on; a sandbox is where a
// project ran this time. The rows live in the shared store, so this tier
// answers these routes directly.

const (
	maxProjectName = 64
	// A backstop against a client bug filling the column, not a considered limit.
	maxPlanBytes = 256 * 1024
)

// registerProjectRoutes mounts the /v1/projects family. Always mounted: the
// store opens in a background goroutine and is nil when Routes() runs, so
// the nil check belongs at request time in projectStore.
func (s *Server) registerProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects", s.auth(s.handleProjectList))
	mux.HandleFunc("POST /v1/projects", s.auth(s.handleProjectCreate))
	mux.HandleFunc("GET /v1/projects/{id}", s.auth(s.handleProjectGet))
	mux.HandleFunc("PATCH /v1/projects/{id}", s.auth(s.handleProjectPatch))
	mux.HandleFunc("DELETE /v1/projects/{id}", s.auth(s.handleProjectArchive))
	mux.HandleFunc("GET /v1/projects/{id}/runs", s.auth(s.handleRunList))
	mux.HandleFunc("POST /v1/projects/{id}/runs", s.auth(s.handleRunCreate))
	mux.HandleFunc("PATCH /v1/projects/{id}/runs/{runID}", s.auth(s.handleRunEnd))
}

// projectStore returns the store, or nil after answering 503.
func (s *Server) projectStore(w http.ResponseWriter) *store.Store {
	st := s.st.Load()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return nil
	}
	return st
}

// project loads the caller's project by id, or answers 404. Scoped by the
// authenticated org so an id guess cannot read another customer's project.
func (s *Server) project(w http.ResponseWriter, r *http.Request, st *store.Store) *store.Project {
	p, err := st.GetProject(r.Context(), fastOrg(r), r.PathValue("id"))
	if err != nil || p == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return nil
	}
	return p
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	limit := httpx.QueryInt(r, "limit", 0) // absent, unparseable or not positive means no limit
	list, err := st.ListProjects(r.Context(), fastOrg(r), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": list})
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	var body struct {
		Name string `json:"name"`
		Plan string `json:"plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, `"name" is required`)
		return
	}
	if len(name) > maxProjectName {
		name = name[:maxProjectName]
	}
	if len(body.Plan) > maxPlanBytes {
		writeErr(w, http.StatusBadRequest, "plan too large")
		return
	}
	org := fastOrg(r)
	p := store.Project{
		ID:        newProjectID(),
		OrgID:     org,
		Owner:     org,
		Name:      name,
		Plan:      body.Plan,
		CreatedAt: time.Now().Unix(),
	}
	if err := st.SaveProject(r.Context(), p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if p := s.project(w, r, st); p != nil {
		writeJSON(w, http.StatusOK, p)
	}
}

// handleProjectPatch renames and re-plans. It cannot touch run history; a run's plan_snapshot is frozen.
func (s *Server) handleProjectPatch(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	p := s.project(w, r, st)
	if p == nil {
		return
	}
	var body struct {
		Name *string `json:"name"`
		Plan *string `json:"plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name != nil {
		n := strings.TrimSpace(*body.Name)
		if n == "" {
			writeErr(w, http.StatusBadRequest, `"name" cannot be empty`)
			return
		}
		if len(n) > maxProjectName {
			n = n[:maxProjectName]
		}
		p.Name = n
	}
	if body.Plan != nil {
		if len(*body.Plan) > maxPlanBytes {
			writeErr(w, http.StatusBadRequest, "plan too large")
			return
		}
		p.Plan = *body.Plan
	}
	if err := st.SaveProject(r.Context(), *p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleProjectArchive archives rather than deletes, so the run history survives.
func (s *Server) handleProjectArchive(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	p := s.project(w, r, st)
	if p == nil {
		return
	}
	if err := st.ArchiveProject(r.Context(), fastOrg(r), p.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	p := s.project(w, r, st)
	if p == nil {
		return
	}
	limit := httpx.QueryInt(r, "limit", 0)
	runs, err := st.ListRuns(r.Context(), fastOrg(r), p.ID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	p := s.project(w, r, st)
	if p == nil {
		return
	}
	var body struct {
		Services []store.RunService `json:"services"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	// The plan is snapshotted, so editing the project later does not rewrite a past run.
	run := store.Run{
		ID:           newRunID(),
		ProjectID:    p.ID,
		OrgID:        fastOrg(r),
		PlanSnapshot: p.Plan,
		Services:     body.Services,
		StartedAt:    time.Now().Unix(),
	}
	if err := st.RecordRun(r.Context(), run); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleRunEnd closes a run.
func (s *Server) handleRunEnd(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if p := s.project(w, r, st); p == nil {
		return
	}
	if err := st.EndRun(r.Context(), r.PathValue("runID"), time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// newProjectID and newRunID have the same shape as sandbox ids (millisecond timestamp plus entropy).
func newProjectID() string { return fmt.Sprintf("pr-%d-%s", time.Now().UnixMilli(), randHex4()) }
func newRunID() string     { return fmt.Sprintf("run-%d-%s", time.Now().UnixMilli(), randHex4()) }

func randHex4() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
