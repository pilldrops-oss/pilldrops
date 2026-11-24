package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// maxTagGrantsPerOrg is a var so a test can lower it.
var maxTagGrantsPerOrg = 100

// tagPushConcurrency bounds how many nodes one tag change pushes to at once.
const tagPushConcurrency = 8

func (s *Server) registerPrivateTagRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/private/grants", s.auth(s.handleTagGrant))
	mux.HandleFunc("GET /v1/private/grants", s.auth(s.handleTagGrantList))
	mux.HandleFunc("DELETE /v1/private/grants/{id}", s.auth(s.handleTagGrantRevoke))
}

func validTag(tag string) bool { return tag != "" && sandbox.ValidNetworks([]string{tag}) == nil }

// tagMembers is the tag's sandboxes that can still run. An ended row keeps its
// tags, so it is skipped here rather than checked against or pushed to.
func tagMembers(ctx context.Context, st *store.Store, org, tag string) ([]store.SandboxRecord, error) {
	ids, err := st.SandboxesInTag(ctx, org, tag)
	if err != nil {
		return nil, err
	}
	var out []store.SandboxRecord
	for _, id := range ids {
		rec, ok, err := st.SandboxByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok && !store.Ended(rec.State) {
			out = append(out, rec)
		}
	}
	return out, nil
}

func memberIDs(members []store.SandboxRecord) []string {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.ID)
	}
	return ids
}

func (s *Server) handleTagGrant(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || privateOff(w) {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		privateGrantRequest
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validTag(body.Tag) {
		writeErr(w, http.StatusBadRequest, `"tag" must be a network tag: lowercase letters, digits and hyphens`)
		return
	}
	g, c, status, err := s.resolveGrant(r.Context(), st, org, body.privateGrantRequest)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	g.SubjectKind, g.Subject = store.GrantSubjectTag, body.Tag
	defer s.privatePushes.lock(grantsLockKey(org))()
	n, err := st.CountTagGrants(r.Context(), org)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n >= maxTagGrantsPerOrg {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("this org already has %d tag grants, the limit; revoke one first", n))
		return
	}
	members, err := tagMembers(r.Context(), st, org, body.Tag)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	unsupported, status, code, err := s.checkTagMembers(r.Context(), st, org, members, g)
	if err != nil {
		writeGrantErr(w, status, code, err)
		return
	}
	stored, err := st.AddGrant(r.Context(), g)
	if errors.Is(err, store.ErrGrantExists) {
		writeJSONErr(w, http.StatusConflict, "private_grant_exists", "this tag already has that grant")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.pushAll(org, memberIDs(members))
	s.auditPrivate(r, st, "private.tag.grant", body.Tag, grantDetail(c.Name, stored))
	out := s.grantJSON(stored, map[string]store.Connector{c.ID: c})
	delete(out, "source")
	out["tag"], out["sandboxes"], out["unsupported"] = body.Tag, len(members), unsupported
	writeJSON(w, http.StatusCreated, out)
}

// checkTagMembers refuses a tag grant that a current member could not route
// beside its other grants, and lists the members whose node cannot carry it:
// they keep the grant and get it once they run on a node that can.
func (s *Server) checkTagMembers(ctx context.Context, st *store.Store, org string, members []store.SandboxRecord, g store.Grant) ([]string, int, string, error) {
	unsupported := []string{}
	for _, rec := range members {
		gs, err := st.EffectiveGrants(ctx, org, rec.ID, recordTags(rec))
		if err != nil {
			return nil, http.StatusInternalServerError, "", err
		}
		if status, code, err := s.checkGrants(ctx, st, append(gs, g)); err != nil {
			return nil, status, code, fmt.Errorf("sandbox %s in tag %s: %w", rec.ID, g.Subject, err)
		}
		if node, ok := s.reg.NodeFor(rec.ID); ok && !nodeCarries(node, g.Kind) {
			unsupported = append(unsupported, rec.ID)
		}
	}
	return unsupported, 0, "", nil
}

// nodeCarries reports whether node can route a grant of kind.
func nodeCarries(node *cluster.Node, kind string) bool {
	if kind == store.GrantKindTarget {
		return node.PrivateAccess
	}
	return node.PrivateRoutes
}

func (s *Server) handleTagGrantList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || privateOff(w) {
		return
	}
	if _, role := callerKey(r.Context()); role == principalSandboxRole {
		writeErr(w, http.StatusForbidden, "a sandbox cannot read tag grants")
		return
	}
	tag := r.URL.Query().Get("tag")
	if tag != "" && !validTag(tag) {
		writeErr(w, http.StatusBadRequest, `"tag" must be a network tag`)
		return
	}
	gs, err := st.TagGrants(r.Context(), fastOrg(r), tag)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cs, err := grantConnectors(r.Context(), st, gs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		m := s.grantJSON(g, cs)
		delete(m, "source")
		m["tag"] = g.Subject
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

func (s *Server) handleTagGrantRevoke(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	g, found, err := st.GrantByID(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found || g.SubjectKind != store.GrantSubjectTag {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	}
	members, err := tagMembers(r.Context(), st, org, g.Subject)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := st.RemoveGrant(r.Context(), g); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.cutRevoked(r.Context(), st, org, members, g)
	s.pushAll(org, memberIDs(members))
	s.auditPrivate(r, st, "private.tag.revoke", g.Subject, grantDetail(g.ConnectorName, g))
	w.WriteHeader(http.StatusNoContent)
}

// pushAll pushes each sandbox's private state in the background, a few at a
// time. Each push takes its sandbox's lock and reads the grants afresh, so a
// grant made meanwhile still lands last.
func (s *Server) pushAll(org string, ids []string) {
	st := s.st.Load()
	if st == nil || len(ids) == 0 {
		return
	}
	go func() {
		sem := make(chan struct{}, tagPushConcurrency)
		var wg sync.WaitGroup
		for _, id := range ids {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := s.pushPrivate(ctx, st, id, org); err != nil {
					s.log().Warn("tag grant not pushed", "component", "private", "sandbox", id, "err", err)
				}
			})
		}
		wg.Wait()
	}()
}

// cutRevoked closes the members' connections that only the revoked grant g
// covered. It runs before the background pushes, so a push that fails cannot
// leave one open.
func (s *Server) cutRevoked(ctx context.Context, st *store.Store, org string, members []store.SandboxRecord, g store.Grant) {
	for _, rec := range members {
		gs, err := st.EffectiveGrants(ctx, org, rec.ID, recordTags(rec))
		if err != nil {
			s.privateConns.closeFor(rec.ID, g.Value)
			continue
		}
		s.privateConns.closeUngranted(rec.ID, gs)
	}
}

// tagJoinAdmin gates putting a sandbox in tags: a tag that carries private
// grants the sandbox does not hold hands them over, so only privateAdmin may.
// sandboxID "" is a new sandbox, which holds nothing. Not skipped with private
// access off: grants joined then would go live once it is turned on.
func (s *Server) tagJoinAdmin(w http.ResponseWriter, r *http.Request, sandboxID string, tags []string) bool {
	st := s.st.Load()
	if st == nil || len(tags) == 0 {
		return true
	}
	ctx, org := r.Context(), fastOrg(r)
	had := map[string]bool{}
	if sandboxID != "" {
		rec, _, err := st.SandboxByID(ctx, sandboxID)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "could not read the sandbox: "+err.Error())
			return false
		}
		held, err := st.EffectiveGrants(ctx, org, sandboxID, recordTags(rec))
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "could not read this org's tag grants: "+err.Error())
			return false
		}
		for _, g := range held {
			had[g.ID] = true
		}
	}
	gs, err := st.EffectiveGrants(ctx, org, sandboxID, tags)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "could not read this org's tag grants: "+err.Error())
		return false
	}
	for _, g := range gs {
		if !had[g.ID] {
			_, ok := s.privateAdmin(w, r)
			return ok
		}
	}
	return true
}

// checkTagChange refuses a network change that would give the sandbox grants
// it cannot route: a conflict, or a kind its node cannot carry. Only the grants
// the change adds must fit the node; ones it already holds were let in before.
func (s *Server) checkTagChange(ctx context.Context, org, sandboxID string, node *cluster.Node, tags []string) (int, string, error) {
	st := s.st.Load()
	if st == nil || !privateAccessOn() {
		return 0, "", nil
	}
	rec, _, err := st.SandboxByID(ctx, sandboxID)
	if err != nil {
		return http.StatusServiceUnavailable, "", err
	}
	held, err := st.EffectiveGrants(ctx, org, sandboxID, recordTags(rec))
	if err != nil {
		return http.StatusServiceUnavailable, "", err
	}
	gs, err := st.EffectiveGrants(ctx, org, sandboxID, tags)
	if err != nil {
		return http.StatusServiceUnavailable, "", err
	}
	if status, code, err := s.checkGrantSet(ctx, st, gs); err != nil {
		return status, code, err
	}
	had := map[string]bool{}
	for _, g := range held {
		had[g.ID] = true
	}
	for _, g := range gs {
		if had[g.ID] || nodeCarries(node, g.Kind) {
			continue
		}
		if g.Kind == store.GrantKindTarget {
			return http.StatusConflict, "private_access_unsupported", errors.New("those tags carry private grants, and " + errPrivateUnsupported.Error())
		}
		return http.StatusConflict, "private_routes_unsupported", errors.New("those tags carry subnet or DNS grants, and " + errRoutesUnsupported.Error())
	}
	return 0, "", nil
}

// checkGrantSet is checkGrants for a set that may add several grants at once,
// as joining tags does: each grant is checked against those before it.
// ponytail: reloads the connectors per grant, at most 50; load once if it shows.
func (s *Server) checkGrantSet(ctx context.Context, st *store.Store, gs []store.Grant) (int, string, error) {
	for i := range gs {
		if status, code, err := s.checkGrants(ctx, st, gs[:i+1]); err != nil {
			return status, code, err
		}
	}
	return 0, "", nil
}
