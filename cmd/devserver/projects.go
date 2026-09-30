package main

// Projects API: a project is the durable thing a user works on, a sandbox is
// where it ran this time. Owner-scoped; a sandbox needs no project.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	maxProjectName = 64
	// A backstop against a client bug filling the column, not a considered limit.
	maxPlanBytes = 256 * 1024
)

// writeJSON encodes v. Headers are already committed when Encode fails, so the
// error is logged rather than answered.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), http.StatusBadRequest)
}

// newID returns a sortable id in the same shape as sandbox ids.
func newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixMilli(), randomHex(4))
}

// registerProjectRoutes mounts the /v1/projects family. Mounts nothing without
// a store: the self-hosted single-node path has none.
func registerProjectRoutes(r chi.Router, st *store.Store, ownerID func(*http.Request) string) {
	if st == nil {
		return
	}

	// Projects are scoped to the owner, like vault secrets and API keys;
	// widening later is easier than narrowing.
	orgOf := func(r *http.Request) string { return ownerID(r) }

	r.Get("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			limit = 0 // no or invalid limit means no limit
		}
		list, err := st.ListProjects(r.Context(), orgOf(r), limit)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"projects": list})
	})

	r.Post("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			Plan string `json:"plan"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid json")
			return
		}
		name := strings.TrimSpace(body.Name)
		// An unnamed project cannot be found again, so the name is required.
		if name == "" {
			badRequest(w, `"name" is required`)
			return
		}
		if len(name) > maxProjectName {
			name = name[:maxProjectName]
		}
		if len(body.Plan) > maxPlanBytes {
			badRequest(w, "plan too large")
			return
		}
		p := store.Project{
			ID:        newID("pr"),
			OrgID:     orgOf(r),
			Owner:     ownerID(r),
			Name:      name,
			Plan:      body.Plan,
			CreatedAt: time.Now().Unix(),
		}
		if err := st.SaveProject(r.Context(), p); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, p)
	})

	r.Get("/v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := st.GetProject(r.Context(), orgOf(r), chi.URLParam(r, "id"))
		if err != nil || p == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, p)
	})

	// Rename and re-plan. Run history is frozen: editing the intent never
	// rewrites what a past run did.
	r.Patch("/v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := st.GetProject(r.Context(), orgOf(r), chi.URLParam(r, "id"))
		if err != nil || p == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		var body struct {
			Name *string `json:"name"`
			Plan *string `json:"plan"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid json")
			return
		}
		if body.Name != nil {
			n := strings.TrimSpace(*body.Name)
			if n == "" {
				badRequest(w, `"name" cannot be empty`)
				return
			}
			if len(n) > maxProjectName {
				n = n[:maxProjectName]
			}
			p.Name = n
		}
		if body.Plan != nil {
			if len(*body.Plan) > maxPlanBytes {
				badRequest(w, "plan too large")
				return
			}
			p.Plan = *body.Plan
		}
		if err := st.SaveProject(r.Context(), *p); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, p)
	})

	// Archive, not delete: removing the record would take the run history with it.
	r.Delete("/v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := st.GetProject(r.Context(), orgOf(r), chi.URLParam(r, "id"))
		if err != nil || p == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if err := st.ArchiveProject(r.Context(), orgOf(r), p.ID); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	r.Get("/v1/projects/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
		p, err := st.GetProject(r.Context(), orgOf(r), chi.URLParam(r, "id"))
		if err != nil || p == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			limit = 0 // no or invalid limit means no limit
		}
		runs, err := st.ListRuns(r.Context(), orgOf(r), p.ID, limit)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"runs": runs})
	})

	r.Post("/v1/projects/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
		p, err := st.GetProject(r.Context(), orgOf(r), chi.URLParam(r, "id"))
		if err != nil || p == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		var body struct {
			PlanSnapshot string             `json:"plan_snapshot"`
			Services     []store.RunService `json:"services"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid json")
			return
		}
		// Fall back to the project's current plan so a run always records what it ran.
		snapshot := body.PlanSnapshot
		if snapshot == "" {
			snapshot = p.Plan
		}
		run := store.Run{
			ID:           newID("run"),
			ProjectID:    p.ID,
			OrgID:        orgOf(r),
			PlanSnapshot: snapshot,
			Services:     body.Services,
			StartedAt:    time.Now().Unix(),
		}
		if err := st.RecordRun(r.Context(), run); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, run)
	})

	// Closing a run is separate from starting it: the finish time is only known later.
	r.Patch("/v1/projects/{id}/runs/{runID}", func(w http.ResponseWriter, r *http.Request) {
		p, err := st.GetProject(r.Context(), orgOf(r), chi.URLParam(r, "id"))
		if err != nil || p == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		found, err := st.EndRun(r.Context(), p.OrgID, p.ID, chi.URLParam(r, "runID"), time.Now().Unix())
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
