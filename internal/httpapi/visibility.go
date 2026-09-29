package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// agentCredentialFiles are the logins a share removes. Handing a machine to
// the org hands over whoever is signed in inside it, and these three files are
// a working session for someone else's Claude, Codex or opencode account.
//
// One way on purpose: unshare restores nothing, because nothing is kept
// anywhere to restore from. Log in again after an unshare.
var agentCredentialFiles = []string{
	"~/.claude/.credentials.json",
	"~/.codex/auth.json",
	"~/.local/share/opencode/auth.json",
}

// shareWipe removes the agent logins and the sandbox's stored env before the
// sandbox becomes visible to the org, and records what it did in resp.
//
// A paused sandbox is woken by the exec (d.Exec resumes first), so the files
// are gone before anyone else can look; only a sandbox that cannot run at all
// records wipe_skipped, and the flip still happens — visibility is the row's
// to decide, and refusing to share a broken sandbox helps nobody.
func (s *Server) shareWipe(r *http.Request, id string, resp map[string]any) {
	res, err := s.d.Exec(r.Context(), id, rt.ExecSpec{
		Cmd:     "rm -f " + strings.Join(agentCredentialFiles, " "),
		Timeout: 30 * time.Second,
	})
	switch {
	case err != nil:
		resp["wipe_skipped"] = err.Error()
	case res.ExitCode != 0:
		resp["wipe_skipped"] = "rm exited " + strconv.Itoa(res.ExitCode) + ": " + res.Stderr
	default:
		resp["wiped"] = agentCredentialFiles
	}
	// The per-sandbox config env holds API keys the dashboard set on it, which
	// the next member would otherwise inherit along with the machine.
	if err := s.st.DeleteSandboxConfig(r.Context(), id); err != nil {
		resp["config_wipe_error"] = err.Error()
	}
}

// handleVisibility flips a sandbox between "org" and "private".
//
// Only the creating key and operators may flip it. A member who can see an
// org-visible sandbox gets 404 here rather than 403: 403 would confirm which
// sandboxes exist and who owns them, which is what visibility is for.
func (s *Server) handleVisibility(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	sb, ok := s.d.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if !operatorPrincipal(r) && !sandbox.CreatedByCaller(sb.CreatedBy, keyIDOf(r)) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Visibility string `json:"visibility"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Visibility == "" {
		writeErr(w, http.StatusBadRequest, `visibility is required: "org" or "private"`)
		return
	}
	want, err := sandbox.ValidVisibility(body.Visibility)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	from := sb.Visibility
	if from == "" {
		from = sandbox.VisibilityOrg
	}

	resp := map[string]any{"id": id, "visibility": want, "was": from}
	// Handing a private sandbox to the org hands over whatever is logged in
	// inside it, so the agent credentials go first and the flip follows. The
	// wipe is one-way: there is nowhere to restore from on unshare.
	switch {
	case from == sandbox.VisibilityPrivate && want == sandbox.VisibilityOrg:
		s.shareWipe(r, id, resp)
	case want == sandbox.VisibilityPrivate:
		// Nothing is wiped going private, and nothing a previous share removed
		// comes back. Re-sharing an already-shared sandbox says neither.
		resp["relogin"] = true
	}
	if err := s.d.SetVisibility(r.Context(), id, want); err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}

	event := "sandbox.unshare"
	if want == sandbox.VisibilityOrg {
		event = "sandbox.share"
	}
	_ = s.st.RecordEvent(r.Context(), store.EventRecord{
		OrgID: orgID(r), SandboxID: id, Event: event, Template: sb.Template,
		ActiveAfter: len(s.d.List(orgID(r))), At: time.Now().Unix(),
	})
	s.auditDetail(r, "sandbox.visibility", id, map[string]any{
		"from": from, "to": want, "wiped": resp["wiped"], "wipe_skipped": resp["wipe_skipped"],
	})
	writeJSON(w, http.StatusOK, resp)
}
