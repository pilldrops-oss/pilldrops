package httpapi

import (
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// handleHistoryList serves GET /v1/history?limit=N&offset=M, newest first, without logs.
func (s *Server) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	limit := httpx.QueryInt(r, "limit", 200)
	offset := httpx.QueryInt(r, "offset", 0)
	recs, err := s.st.ListHistoryPage(r.Context(), orgID(r), limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if recs == nil {
		recs = []store.HistoryRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": recs})
}

// handleHistoryGet serves GET /v1/history/{id}, including retained logs.
func (s *Server) handleHistoryGet(w http.ResponseWriter, r *http.Request) {
	rec, ok, err := s.st.GetHistory(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok || rec.OrgID != orgID(r) {
		writeErr(w, http.StatusNotFound, "history record not found")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
