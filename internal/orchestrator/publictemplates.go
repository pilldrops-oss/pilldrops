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
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes()) //nolint:errcheck // best-effort: headers are committed
		return
	}
	now := s.now()
	for _, t := range cat {
		id, _ := t["id"].(string)
		// The same Request a create of this template places with.
		place := cluster.Request{Template: id, Runtime: templateRuntime(id), NoBuild: s.noReadyBuild(r.Context(), id)}
		t["available"], _ = s.reg.TemplateSupport(place, now, cluster.NodeTTL)
	}
	writeJSON(w, http.StatusOK, cat)
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
	cat := templates.Catalog()
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
