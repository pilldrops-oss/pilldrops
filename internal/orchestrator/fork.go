package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
)

// handleFork forwards a fork to the parent's node and places the child there.
// Admitted like a create: the child is one more machine against the plan.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	if !ok {
		writeErr(w, 404, "sandbox not found")
		return
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, 400, "read body: "+err.Error())
			return
		}
		body = b
	}
	releaseSlot, msg, ok := s.admitBody(r, body)
	if !ok {
		if msg == admissionUnavailable {
			writeErr(w, 503, msg)
			return
		}
		writeErr(w, 429, msg)
		return
	}
	defer releaseSlot()
	resp, err := s.forward(r.Context(), node, "POST", "/v1/sandboxes/"+id+"/fork", body, r.Header.Get("Content-Type"), fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node fork failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // response fully consumed below
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		writeErr(w, 502, "reading node fork response: "+err.Error())
		return
	}
	if resp.StatusCode == 201 {
		var created struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		if json.Unmarshal(rb, &created) == nil && created.ID != "" {
			s.reg.Place(created.ID, node.ID)
			// Called even with no url: the node's other *_url fields carry its
			// own ?ptok= and must not be relayed either. See sanitizeNodeURLs.
			to, viewer := s.previewLinks(r, created.URL)
			rb = s.rewriteURL(r, rb, created.URL, to, viewer, node.ID)
			rb = s.withPreviewGeneration(r.Context(), rb, created.ID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb) //nolint:errcheck // status already committed
}
