package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// setPublicPreview records whether id's preview skips the token gate, in the
// store when there is one and the in-memory fallback when there is not, and
// forgets this replica's cached answer so the change applies on the next request.
func (s *Server) setPublicPreview(ctx context.Context, id string, public bool) error {
	if st := s.st.Load(); st != nil {
		if err := st.SetPublicPreview(ctx, id, public); err != nil {
			return err
		}
		s.pcache.drop("pub:" + id)
		return nil
	}
	if public {
		s.publicSB.Store(id, true)
	} else {
		s.publicSB.Delete(id)
	}
	return nil
}

// handlePreviewAccess turns a sandbox's tokenless preview on or off after
// launch. Only its creator and operators may; anyone else gets 404, as on the
// visibility flip, so the answer does not confirm whose machine it is.
//
// Either flip moves the preview generation, so turning it off also retracts
// every link and cookie handed out while it was on. The terminal and desktop
// ports stay token-gated either way (authorizePreview).
func (s *Server) handlePreviewAccess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Public *bool `json:"public"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Public == nil {
		writeErr(w, http.StatusBadRequest, `public is required: true or false`)
		return
	}
	if _, placed := s.reg.NodeFor(id); !placed {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	keyID, keyRole := callerKey(r.Context())
	operator := keyRole == "operator" || (isOperator(r) && keyID == "")
	if st := s.st.Load(); st != nil {
		row, found, err := st.GetActiveSandboxRow(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "cannot read the sandbox: "+err.Error())
			return
		}
		if !found || row.OrgID != fastOrg(r) || (!operator && !sandbox.CreatedByCaller(row.CreatedBy, keyID)) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
	}
	was := s.isPublicPreview(r.Context(), id)
	if err := s.setPublicPreview(r.Context(), id, *body.Public); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "could not record preview access: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "public_preview": *body.Public, "was": was})
}
