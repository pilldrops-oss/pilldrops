package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// registerPublicTemplateRoutes mounts the unauthenticated catalogue for the
// signed-out picker. It is the only /v1 route outside s.auth, which is why
// publicCatalog must redact.
func (s *Server) registerPublicTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/templates/public", s.handlePublicTemplates)
	mux.HandleFunc("GET /v1/templates", s.auth(s.handleTemplates))
}

// handleTemplates relays a node's catalogue and marks each entry with
// available: whether some healthy, uncordoned node can boot it, by the
// scheduler's own rule. /v1/nodes is operator-only, so this is the only way a
// tenant can grey out a template no node will take; it names no node.
func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	s.proxyToNode(rec, r, http.MethodGet, r.URL.Path)
	var cat []map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &cat) != nil {
		if cat = s.compiledCatalog(r.Context(), rec.Code); cat == nil {
			maps.Copy(w.Header(), rec.Header())
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes()) //nolint:errcheck // best-effort: headers are committed
			return
		}
	}
	now := s.now()
	// An older node lists internal templates; they never reach a tenant.
	listed := make([]map[string]any, 0, len(cat))
	for _, t := range cat {
		id, _ := t["id"].(string)
		if templates.Internal(id) {
			continue
		}
		// The same Request a create of this template places with.
		place := cluster.Request{Template: id, Runtime: templateRuntime(id), NoBuild: s.noReadyBuild(r.Context(), id)}
		t["available"], _ = s.reg.TemplateSupport(place, now, cluster.NodeTTL)
		listed = append(listed, t)
	}
	for _, t := range s.customTemplates(r) {
		t["available"], _ = s.reg.TemplateSupport(cluster.Request{Template: t["id"].(string)}, now, cluster.NodeTTL)
		listed = append(listed, t)
	}
	writeJSON(w, http.StatusOK, listed)
}

// compiledCatalog stands in when no node answered; nil without the store, which holds what the admin unpublished.
func (s *Server) compiledCatalog(ctx context.Context, code int) []map[string]any {
	st := s.st.Load()
	if st == nil || (code != http.StatusServiceUnavailable && code != http.StatusBadGateway) {
		return nil
	}
	flags, err := st.GetImageFlags(ctx)
	if err != nil {
		s.log().Warn("could not read image flags", "component", "templates", "err", err)
		return nil
	}
	published := make([]templates.Template, 0, len(templates.Listed()))
	for _, t := range templates.Listed() {
		if f, ok := flags[t.ID]; ok && f.Published != nil && !*f.Published {
			continue
		}
		published = append(published, t)
	}
	raw, err := json.Marshal(published)
	if err != nil {
		return nil
	}
	var cat []map[string]any
	if json.Unmarshal(raw, &cat) != nil {
		return nil
	}
	return cat
}

// customTemplates is what the caller's org built, which no node's catalogue
// lists: the catalogue is compiled in and shared by every org.
func (s *Server) customTemplates(r *http.Request) []map[string]any {
	st := s.st.Load()
	org := fastOrg(r)
	if st == nil || org == "" {
		return nil
	}
	built, err := st.CustomTemplatesForOrg(r.Context(), org)
	if err != nil {
		s.log().Warn("could not list custom templates", "component", "templates", "err", err)
		return nil
	}
	out := make([]map[string]any, 0, len(built))
	for _, b := range built {
		out = append(out, map[string]any{
			"id": b.TemplateID, "name": b.TemplateID, "custom": true, "published": true,
			"size_mb": b.TotalSize >> 20, "updated_at": b.ReadyAt,
		})
	}
	return out
}

// handleImagesList relays a node's image list without its internal templates,
// which an older node still lists.
func (s *Server) handleImagesList(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	s.proxyToNode(rec, r, http.MethodGet, "/v1/images")
	var page struct {
		Images []map[string]any `json:"images"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes()) //nolint:errcheck // best-effort: headers are committed
		return
	}
	listed := make([]map[string]any, 0, len(page.Images))
	for _, img := range page.Images {
		if id, _ := img["id"].(string); !templates.Internal(id) {
			listed = append(listed, img)
		}
	}
	listed = append(listed, s.customTemplates(r)...)
	writeJSON(w, http.StatusOK, map[string]any{"images": listed})
}

// noReadyBuild reports that the store knows tmpl has no ready build, so a node
// that would have to fetch it cannot boot it. Without a store, or on a read
// error, it says false and leaves the answer to the node.
func (s *Server) noReadyBuild(ctx context.Context, tmpl string) bool {
	st := s.st.Load()
	if st == nil || tmpl == "" {
		return false
	}
	_, err := st.ReadyBuildForTemplate(ctx, tmpl)
	return errors.Is(err, store.ErrNoReadyBuild)
}

func (s *Server) handlePublicTemplates(w http.ResponseWriter, r *http.Request) {
	// The catalogue is compiled in and changes only on deploy.
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, publicCatalog())
}

// publicCatalog strips env values (names and flags stay so the UI can render a
// form). Env must be copied: the slice shares the catalogue's backing array,
// so redacting in place would strip the authenticated endpoint too.
func publicCatalog() []templates.Template {
	cat := templates.Listed()
	out := make([]templates.Template, 0, len(cat))
	for _, t := range cat {
		if len(t.Env) > 0 {
			env := make([]templates.TemplateEnv, len(t.Env))
			for i, e := range t.Env {
				env[i] = templates.TemplateEnv{Name: e.Name, Required: e.Required, Secret: e.Secret}
			}
			t.Env = env
		}
		out = append(out, t)
	}
	return out
}
