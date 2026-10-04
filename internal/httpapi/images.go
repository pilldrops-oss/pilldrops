package httpapi

import (
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// ImageInfo is the admin view of a template returned by GET /v1/images and PATCH /v1/images/{id}.
type ImageInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Port        int      `json:"port"`
	VCPU        int      `json:"vcpu"`
	MemoryMB    int      `json:"memory_mb"`
	SizeMB      int      `json:"size_mb"`
	Published   bool     `json:"published"`
	Featured    bool     `json:"featured"`
	Language    string   `json:"language"`
	Tags        []string `json:"tags"`
	UpdatedAt   int64    `json:"updated_at"`
}

// imageInfo layers stored curation flags over the discovered template.
func imageInfo(t templates.Template, flags store.ImageFlags, updatedAt int64) ImageInfo {
	info := ImageInfo{
		ID: t.ID, Name: t.Name, Description: t.Description, Port: t.Port,
		VCPU: t.CPU, MemoryMB: t.MemoryMB, SizeMB: templates.SizeMB(t.ID),
		Published: true, Featured: flags.Featured,
		Language: t.Language, Tags: t.Tags, UpdatedAt: updatedAt,
	}
	if info.Tags == nil {
		info.Tags = []string{}
	}
	if flags.Published != nil {
		info.Published = *flags.Published
	}
	if flags.Name != "" {
		info.Name = flags.Name
	}
	if flags.Description != "" {
		info.Description = flags.Description
	}
	return info
}

// handleImages lists every template with its curation flags.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	flags, err := s.st.GetImageFlags(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	now := time.Now().Unix()
	cat := templates.Listed()
	out := make([]ImageInfo, 0, len(cat))
	for _, t := range cat {
		out = append(out, imageInfo(t, flags[t.ID], now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": out})
}

// handleImagePatch stores curation overrides for one image and returns the updated ImageInfo.
func (s *Server) handleImagePatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := templates.Resolve(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown image: "+id)
		return
	}
	var body struct {
		Published   *bool  `json:"published"`
		Featured    bool   `json:"featured"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	flags, err := s.st.SetImageFlags(r.Context(), t.ID, store.ImageFlags{
		Published: body.Published, Featured: body.Featured,
		Name: body.Name, Description: body.Description,
	})
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, imageInfo(t, flags, time.Now().Unix()))
}
