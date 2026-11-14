package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func (s *Server) registerPrivateGrantRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sandboxes/{id}/private", s.auth(s.bySlug(s.handlePrivateGrant)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/private", s.auth(s.bySlug(s.handlePrivateGrantList)))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/private/{connector}/{target}", s.auth(s.bySlug(s.handlePrivateRevoke)))
}

// ownSandbox loads the caller's sandbox; another org's id reads as not found.
func (s *Server) ownSandbox(w http.ResponseWriter, r *http.Request, st *store.Store, org string) (store.SandboxRecord, bool) {
	rec, ok, err := st.SandboxByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return store.SandboxRecord{}, false
	}
	if !ok || rec.OrgID != org {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return store.SandboxRecord{}, false
	}
	return rec, true
}

func (s *Server) privateTargetsFor(ctx context.Context, st *store.Store, sandboxID string) ([]sandbox.PrivateTarget, error) {
	gs, err := st.PrivateGrants(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.PrivateTarget, 0, len(gs))
	for _, g := range gs {
		c, ok, err := st.ConnectorByID(ctx, "", g.ConnectorID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for _, t := range s.connectors.union(c.ID, c.Targets) {
			if t.Name == g.Target {
				out = append(out, sandbox.PrivateTarget{Name: t.Name, Host: t.Host, Port: t.Port})
			}
		}
	}
	return out, nil
}

// pushPrivate stores the sandbox's routed targets and sends them to its node.
// status is the node's reply, 0 when the sandbox has no node right now; the
// stored list is applied when it next resumes.
func (s *Server) pushPrivate(ctx context.Context, st *store.Store, sandboxID, org string) (int, error) {
	ts, err := s.privateTargetsFor(ctx, st, sandboxID)
	if err != nil {
		return 0, err
	}
	stored, _ := json.Marshal(ts)
	if err := st.SetSandboxPrivate(ctx, sandboxID, string(stored)); err != nil {
		return 0, err
	}
	node, ok := s.reg.NodeFor(sandboxID)
	if !ok {
		return 0, nil
	}
	body, _ := json.Marshal(map[string]any{"private": ts})
	resp, err := s.forward(ctx, node, "PATCH", "/v1/sandboxes/"+sandboxID, body, "application/json", org)
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body read below
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if bytes.Contains(b, []byte("private_access_unsupported")) {
			return http.StatusConflict, errors.New("private access is not available on this sandbox's node")
		}
		s.log().Warn("node refused a private targets change", "component", "private", "sandbox", sandboxID, "status", resp.StatusCode, "body", string(b))
		return resp.StatusCode, errors.New("the sandbox's node refused the change")
	}
	return resp.StatusCode, nil
}

// grantPrivate stores one grant and pushes it to the node, rolling it back if
// the push fails. code is empty for plain errors.
func (s *Server) grantPrivate(ctx context.Context, st *store.Store, org, sandboxID, connector, target string) (c store.Connector, status int, code string, err error) {
	c, found, err := st.ConnectorByName(ctx, org, connector)
	if err != nil {
		return c, http.StatusInternalServerError, "", err
	}
	if !found {
		return c, http.StatusNotFound, "", errors.New("connector not found")
	}
	announced := false
	for _, t := range s.connectors.union(c.ID, c.Targets) {
		announced = announced || t.Name == target
	}
	if !announced {
		return c, http.StatusNotFound, "", errors.New("connector " + c.Name + " has not announced target " + target)
	}
	g := store.PrivateGrant{SandboxID: sandboxID, ConnectorID: c.ID, Target: target, CreatedAt: s.now().Unix()}
	if err := st.AddPrivateGrant(ctx, g); errors.Is(err, store.ErrPrivateNameTaken) {
		return c, http.StatusConflict, "private_name_taken", errors.New("this sandbox already reaches a target named " + target + "; remove that grant first")
	} else if err != nil {
		return c, http.StatusInternalServerError, "", err
	}
	status, err = s.pushPrivate(ctx, st, sandboxID, org)
	if err == nil {
		return c, 0, "", nil
	}
	if _, rmErr := st.RemovePrivateGrant(ctx, sandboxID, c.ID, target); rmErr != nil {
		s.log().Warn("could not roll back a private grant", "component", "private", "sandbox", sandboxID, "target", target, "err", rmErr)
	}
	if _, pushErr := s.pushPrivate(ctx, st, sandboxID, org); pushErr != nil {
		s.log().Warn("could not re-push private targets after a rollback", "component", "private", "sandbox", sandboxID, "err", pushErr)
	}
	code = "private_push_failed"
	if status == http.StatusConflict {
		code = "private_access_unsupported"
	}
	if status == 0 {
		status = http.StatusBadGateway
	}
	return c, status, code, err
}

func writeGrantErr(w http.ResponseWriter, status int, code string, err error) {
	if code == "" {
		writeErr(w, status, err.Error())
		return
	}
	writeJSONErr(w, status, code, err.Error())
}

func (s *Server) handlePrivateGrant(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || privateOff(w) {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	rec, ok := s.ownSandbox(w, r, st, org)
	if !ok {
		return
	}
	var body struct {
		Connector string `json:"connector"`
		Target    string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	c, status, code, err := s.grantPrivate(r.Context(), st, org, rec.ID, body.Connector, body.Target)
	if err != nil {
		writeGrantErr(w, status, code, err)
		return
	}
	s.auditPrivate(r, st, "private.grant", rec.ID, c.Name+"/"+body.Target)
	writeJSON(w, http.StatusCreated, map[string]any{"connector": c.Name, "target": body.Target,
		"names": privateNames(s.connectors.union(c.ID, c.Targets), body.Target)})
}

func privateNames(ts []store.ConnectorTarget, target string) []string {
	out := []string{target + ".private"}
	for _, t := range ts {
		if t.Name == target && t.Host != "" {
			out = append(out, t.Host)
		}
	}
	return out
}

func (s *Server) handlePrivateGrantList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if _, role := callerKey(r.Context()); role == principalSandboxRole {
		writeErr(w, http.StatusForbidden, "a sandbox cannot read its grants")
		return
	}
	rec, ok := s.ownSandbox(w, r, st, fastOrg(r))
	if !ok {
		return
	}
	gs, err := st.PrivateGrants(r.Context(), rec.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		c, _, _ := st.ConnectorByID(r.Context(), "", g.ConnectorID)
		out = append(out, map[string]any{"connector": g.ConnectorName, "target": g.Target, "created_at": g.CreatedAt,
			"online": s.connectors.count(g.ConnectorID) > 0, "names": privateNames(s.connectors.union(c.ID, c.Targets), g.Target)})
	}
	node, placed := s.reg.NodeFor(rec.ID)
	writeJSON(w, http.StatusOK, map[string]any{"grants": out, "supported": !placed || node.PrivateAccess})
}

func (s *Server) handlePrivateRevoke(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	rec, ok := s.ownSandbox(w, r, st, org)
	if !ok {
		return
	}
	c, found, err := st.ConnectorByName(r.Context(), org, r.PathValue("connector"))
	if err != nil || !found {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	}
	done, err := st.RemovePrivateGrant(r.Context(), rec.ID, c.ID, r.PathValue("target"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !done {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	}
	s.privateConns.closeFor(rec.ID, r.PathValue("target"))
	if _, err := s.pushPrivate(r.Context(), st, rec.ID, org); err != nil {
		s.log().Warn("private revoke not pushed", "component", "private", "sandbox", rec.ID, "err", err)
	}
	s.auditPrivate(r, st, "private.revoke", rec.ID, c.Name+"/"+r.PathValue("target"))
	w.WriteHeader(http.StatusNoContent)
}

type privateGrantRequest struct {
	Connector string `json:"connector"`
	Target    string `json:"target"`
}

// hasPrivateTargets reports whether a stored private_targets list names any target.
func hasPrivateTargets(raw string) bool { return raw != "" && raw != "[]" }

// stripPrivate removes the control-plane-only "private" key before the body goes
// to a node. Any spelling: the node's JSON decoding matches keys case-insensitively.
func stripPrivate(w http.ResponseWriter, body []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return nil, false
	}
	for k := range fields {
		if strings.EqualFold(k, "private") {
			delete(fields, k)
		}
	}
	out, _ := json.Marshal(fields)
	return out, true
}

// grantAtCreate grants a new sandbox its private targets; on any failure the
// sandbox is destroyed, so a create with "private" is all or nothing.
func (s *Server) grantAtCreate(ctx context.Context, node *cluster.Node, sandboxID, org string, grants []privateGrantRequest) (int, string, error) {
	st := s.st.Load()
	for _, g := range grants {
		if st == nil {
			break
		}
		if _, status, code, err := s.grantPrivate(ctx, st, org, sandboxID, g.Connector, g.Target); err != nil {
			s.destroyNew(ctx, node, sandboxID, org)
			return status, code, err
		}
	}
	return 0, "", nil
}

// destroyNew removes a sandbox whose grants failed. The node's answer is only
// logged: local cleanup runs either way so no grant or route outlives it.
func (s *Server) destroyNew(ctx context.Context, node *cluster.Node, sandboxID, org string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	resp, err := s.forward(ctx, node, "DELETE", "/v1/sandboxes/"+sandboxID, nil, "", org)
	switch {
	case err != nil:
		s.log().Warn("could not destroy a sandbox whose grants failed", "component", "private", "sandbox", sandboxID, "err", err)
	case resp.StatusCode >= 300:
		s.log().Warn("node refused to destroy a sandbox whose grants failed", "component", "private", "sandbox", sandboxID, "status", resp.StatusCode)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if st := s.st.Load(); st != nil {
		_ = st.ForgetSandbox(ctx, sandboxID) //nolint:errcheck // best-effort: the node removes its own row too
	}
	s.reg.Unplace(sandboxID)
	s.dropPrivate(ctx, sandboxID)
}

// dropPrivate forgets a destroyed sandbox's grants and cuts its open connections.
func (s *Server) dropPrivate(ctx context.Context, sandboxID string) {
	if st := s.st.Load(); st != nil {
		_ = st.DeletePrivateGrantsForSandbox(ctx, sandboxID) //nolint:errcheck // best-effort: the sandbox is gone, orphan grants route nowhere
	}
	s.privateConns.closeFor(sandboxID, "")
}
