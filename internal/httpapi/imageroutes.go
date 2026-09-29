package httpapi

import "net/http"

// registerImageRoutes mounts the image and template catalogue surface.
func (s *Server) registerImageRoutes(route func(string, http.Handler)) {
	route("GET /v1/templates", s.auth(s.handleTemplates))
	route("GET /v1/images", s.auth(s.handleImages))
	route("POST /v1/images/build", s.auth(s.handleImageBuild))
	// authAdmin: export returns an image's full contents and the caller names the tag.
	route("GET /v1/images/export", s.authAdmin(s.handleImageExport))
	// authAdmin: the catalog flags are shared by every org.
	route("PATCH /v1/images/{id}", s.authAdmin(s.handleImagePatch))
}
