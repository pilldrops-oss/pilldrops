package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.st.ListKeys(r.Context(), orgID(r))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	// A member never sees the fleet's operator and node keys in its org.
	if !operatorPrincipal(r) {
		keys = slices.DeleteFunc(keys, func(k store.KeyInfo) bool { return k.Role == "operator" })
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" {
		body.Name = "key"
	}
	rec, plaintext, err := s.st.CreateKey(r.Context(), body.Name, orgID(r))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.audit(r, "key.create", rec.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": rec.ID, "name": rec.Name, "key": plaintext,
		"masked_key": "sf_live_****" + rec.ID[len(rec.ID)-4:],
		"created_at": time.Now().Unix(),
	})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Org-scoped so one tenant cannot revoke another's key. Another org's key
	// reads as 404, not 403, so the id's existence is not leaked.
	if err := s.st.RevokeKey(r.Context(), id, orgID(r), operatorPrincipal(r)); err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			writeErr(w, http.StatusNotFound, "key not found")
			return
		}
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	// Revoked keys die immediately on this node; other nodes age out within the auth-cache TTL.
	s.authc.Clear()
	s.audit(r, "key.revoke", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	// Independent store reads, run concurrently to keep dashboard refresh fast.
	org := orgID(r)
	var (
		sum      store.UsageSummary
		sumErr   error
		byClient map[string]int
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sum, sumErr = s.st.UsageSummary(r.Context(), org)
	}()
	go func() {
		defer wg.Done()
		byClient, _ = s.st.CreationsByClient(r.Context(), org)
	}()
	points, active := s.st.UsageSeries(r.Context(), org)
	wg.Wait()
	if sumErr != nil {
		writeErr(w, http.StatusServiceUnavailable, sumErr.Error())
		return
	}
	if byClient == nil {
		byClient = map[string]int{}
	}
	// Hours use 4 decimals so seconds-scale usage does not round to 0.00.
	writeJSON(w, http.StatusOK, map[string]any{
		// Distinct metered sandboxes from the store, not the live count.
		"total_starts": sum.Sandboxes,
		// Fleet-wide, not just this node.
		"active":       active,
		"vcpu_hours":   round4(sum.VCPUSeconds / 3600),
		"ram_gb_hours": round4(sum.RAMGBSeconds / 3600),
		"points":       points,
		"by_client":    byClient,
	})
}

func round4(f float64) float64 { return float64(int(f*10000)) / 10000 }

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	// Customer-facing, so hide images the admin unpublished. No flag means published.
	flags, err := s.st.GetImageFlags(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	cat := templates.Catalog()
	out := make([]templates.Template, 0, len(cat))
	for _, t := range cat {
		if f, ok := flags[t.ID]; ok && f.Published != nil && !*f.Published {
			continue
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUnbuilt refuses a route whose feature does not exist. These used to
// answer 200 with an empty list, which is indistinguishable from "you have
// none" and told every client the feature was there and idle. There is no
// store table behind any of them, so there is nothing that could ever be
// listed; a caller needs to know that, not to keep polling.
func (s *Server) handleUnbuilt(w http.ResponseWriter, r *http.Request) {
	writeErrCode(w, http.StatusNotImplemented, "not_built",
		r.URL.Path+" is not implemented on this platform — it has no backing store, "+
			"so it can only ever be empty. Do not poll it.")
}

// handleAdminSandboxList returns every sandbox on this node across all orgs,
// in the GET /v1/sandboxes wire shape. The orchestrator's placement reconciler needs it.
func (s *Server) handleAdminSandboxList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": s.d.List("")})
}
