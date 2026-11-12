package httpapi

import (
	"errors"
	"net/http"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func (s *Server) registerVolumeRoutes(route func(string, http.Handler)) {
	route("POST /v1/volumes", s.auth(s.handleCreateVolume))
	route("GET /v1/volumes", s.auth(s.handleListVolumes))
	route("GET /v1/volumes/{id}", s.auth(s.handleGetVolume))
	route("DELETE /v1/volumes/{id}", s.auth(s.handleDeleteVolume))
}

// volumeStatus maps a volume error to its status, 0 when err is not one.
func volumeStatus(err error) int {
	switch {
	case errors.Is(err, daemon.ErrVolumeNotFound):
		return http.StatusNotFound
	case errors.Is(err, daemon.ErrHasVolumes), errors.Is(err, store.ErrVolumeAttached):
		return http.StatusConflict
	case errors.Is(err, daemon.ErrVolumesUnsupported):
		return http.StatusNotImplemented
	}
	return 0
}

func volumeMounts(body createReq) []rt.VolumeMount {
	var out []rt.VolumeMount
	for _, v := range body.Volumes {
		out = append(out, rt.VolumeMount{ID: v.ID, Path: v.Path})
	}
	return out
}

func volumeErr(w http.ResponseWriter, err error) {
	var inv *sandbox.InvalidError
	switch {
	case errors.As(err, &inv):
		writeErr(w, http.StatusBadRequest, inv.Reason)
	case volumeStatus(err) != 0:
		writeErr(w, volumeStatus(err), err.Error())
	default:
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	}
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		SizeMB int    `json:"size_mb"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	v, err := s.d.CreateVolume(r.Context(), orgID(r), body.Name, body.SizeMB)
	if err != nil {
		volumeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	vols, err := s.d.ListVolumes(r.Context(), orgID(r))
	if err != nil {
		volumeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
}

func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	v, err := s.d.GetVolume(r.Context(), orgID(r), r.PathValue("id"))
	if err != nil {
		volumeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	if err := s.d.DeleteVolume(r.Context(), orgID(r), r.PathValue("id")); err != nil {
		volumeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
