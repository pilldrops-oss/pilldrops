package orchestrator

import (
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// registerPublicTemplateRoutes mounts the unauthenticated catalogue for the
// signed-out picker. It is the only /v1 route outside s.auth, which is why
// publicCatalog must redact.
func (s *Server) registerPublicTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/templates/public", s.handlePublicTemplates)
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
