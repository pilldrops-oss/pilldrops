package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func (s *Server) registerPrivateGrantRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sandboxes/{id}/private", s.auth(s.bySlug(s.handlePrivateGrant)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/private", s.auth(s.bySlug(s.handlePrivateGrantList)))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/private/{grant}", s.auth(s.bySlug(s.handlePrivateRevokeID)))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/private/{connector}/{target}", s.auth(s.bySlug(s.handlePrivateRevoke)))
}

var (
	errPrivateUnsupported = errors.New("private access is not available on this sandbox's node")
	errRoutesUnsupported  = errors.New("subnet and DNS grants are not available on this sandbox's node")
)

// privateGrantRequest names one connector and exactly one of a target, a
// subnet or a DNS suffix. A create's "private" list and the grant routes share it.
type privateGrantRequest struct {
	Connector string `json:"connector"`
	Target    string `json:"target,omitempty"`
	Subnet    string `json:"subnet,omitempty"`
	DNS       string `json:"dns,omitempty"`
}

// kindValue is the kind and canonical value the request names.
func (g privateGrantRequest) kindValue() (kind, value string, err error) {
	n := 0
	for _, v := range []string{g.Target, g.Subnet, g.DNS} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return "", "", errors.New(`name exactly one of "target", "subnet" or "dns"`)
	}
	switch {
	case g.Target != "":
		return store.GrantKindTarget, g.Target, nil
	case g.Subnet != "":
		p, err := sandbox.ParsePrivateSubnet(g.Subnet)
		if err != nil {
			return "", "", err
		}
		return store.GrantKindSubnet, p.String(), nil
	}
	d := strings.TrimSuffix(strings.ToLower(g.DNS), ".")
	if err := sandbox.ValidPrivateSuffix(d); err != nil {
		return "", "", err
	}
	return store.GrantKindDNS, d, nil
}

// recordTags is a sandbox row's network tags; an unreadable list reads as none.
func recordTags(rec store.SandboxRecord) []string {
	var tags []string
	_ = json.Unmarshal([]byte(rec.Networks), &tags) //nolint:errcheck // "" and garbage both mean no tags
	return tags
}

// grantConnectors loads each connector the grants name, once.
func grantConnectors(ctx context.Context, st *store.Store, gs []store.Grant) (map[string]store.Connector, error) {
	out := map[string]store.Connector{}
	for _, g := range gs {
		if _, ok := out[g.ConnectorID]; ok {
			continue
		}
		c, found, err := st.ConnectorByID(ctx, "", g.ConnectorID)
		if err != nil {
			return nil, err
		}
		if found {
			out[g.ConnectorID] = c
		}
	}
	return out, nil
}

// targetOf is a target grant as the node routes it, from what its connector announces now.
func (s *Server) targetOf(cs map[string]store.Connector, g store.Grant) (sandbox.PrivateTarget, bool) {
	c, ok := cs[g.ConnectorID]
	if !ok || g.Kind != store.GrantKindTarget {
		return sandbox.PrivateTarget{}, false
	}
	for _, t := range s.connectors.union(c.ID, c.Targets) {
		if t.Name == g.Value {
			return sandbox.PrivateTarget{Name: t.Name, Host: t.Host, Port: t.Port}, true
		}
	}
	return sandbox.PrivateTarget{}, false
}

// routedState is what a sandbox's effective grants route: the phase 1 target
// list, and the subnet and DNS routes with the platform ranges removed. A
// grant held both directly and through a tag routes once.
func (s *Server) routedState(cs map[string]store.Connector, gs []store.Grant) ([]sandbox.PrivateTarget, sandbox.PrivateRoutes) {
	ts := []sandbox.PrivateTarget{}
	seen := map[string]bool{}
	var nets []netip.Prefix
	names := map[string]bool{}
	for _, g := range gs {
		switch g.Kind {
		case store.GrantKindTarget:
			if t, ok := s.targetOf(cs, g); ok && !seen[t.Name] {
				seen[t.Name] = true
				ts = append(ts, t)
			}
		case store.GrantKindSubnet:
			if p, err := netip.ParsePrefix(g.Value); err == nil {
				nets = append(nets, p)
			}
		case store.GrantKindDNS:
			names[g.Value] = true
		}
	}
	routes := sandbox.PrivateRoutes{Subnets: []string{}, DNS: slices.Sorted(maps.Keys(names))}
	if routes.DNS == nil {
		routes.DNS = []string{}
	}
	for _, p := range sandbox.CarvePlatform(nets) {
		routes.Subnets = append(routes.Subnets, p.String())
	}
	return ts, routes
}

// grantConflict explains why the last grant in gs cannot route beside the
// others, "" when it can: one target name or hostname from two connectors,
// overlapping subnets from two connectors, or one suffix from two connectors.
// A conflict among the earlier grants alone is not reported. hostOf is a
// target grant's announced hostname.
func grantConflict(gs []store.Grant, hostOf func(store.Grant) string) (code, msg string) {
	if len(gs) == 0 {
		return "", ""
	}
	n := gs[len(gs)-1]
	for _, o := range gs[:len(gs)-1] {
		if o.Kind != n.Kind {
			continue
		}
		switch n.Kind {
		case store.GrantKindTarget:
			if o.Value == n.Value && o.ConnectorID != n.ConnectorID {
				return "private_name_taken", "this sandbox already reaches a target named " + n.Value + "; remove that grant first"
			}
			// Both would resolve to one address in the sandbox, silently reaching only one of them.
			if h := hostOf(n); h != "" && o.Value != n.Value && hostOf(o) == h {
				return "private_name_taken", "this sandbox already reaches " + h + " through target " + o.Value + "; remove that grant first"
			}
		case store.GrantKindSubnet:
			p, err1 := netip.ParsePrefix(n.Value)
			op, err2 := netip.ParsePrefix(o.Value)
			if err1 == nil && err2 == nil && o.ConnectorID != n.ConnectorID && op.Overlaps(p) {
				return "private_route_conflict", n.Value + " overlaps " + o.Value + ", which this sandbox already reaches through another connector"
			}
		case store.GrantKindDNS:
			if o.Value == n.Value && o.ConnectorID != n.ConnectorID {
				return "private_route_conflict", "this sandbox already resolves " + n.Value + " through another connector"
			}
		}
	}
	return "", ""
}

// checkGrants refuses the last of gs, a sandbox's effective grants plus a new one, when the sandbox cannot route it.
func (s *Server) checkGrants(ctx context.Context, st *store.Store, gs []store.Grant) (int, string, error) {
	if len(gs) > sandbox.MaxEffectiveGrants {
		return http.StatusTooManyRequests, "", fmt.Errorf("a sandbox may hold at most %d private grants, counting its tags'", sandbox.MaxEffectiveGrants)
	}
	cs, err := grantConnectors(ctx, st, gs)
	if err != nil {
		return http.StatusInternalServerError, "", err
	}
	hostOf := func(g store.Grant) string {
		t, _ := s.targetOf(cs, g)
		return normalHost(t.Host)
	}
	if code, msg := grantConflict(gs, hostOf); code != "" {
		return http.StatusConflict, code, errors.New(msg)
	}
	return 0, "", nil
}

// announced checks that connector c announces what a grant names: the target,
// a subnet containing the granted one, or a suffix at or above the granted one.
func (s *Server) announced(c store.Connector, kind, value string) error {
	switch kind {
	case store.GrantKindTarget:
		for _, t := range s.connectors.union(c.ID, c.Targets) {
			if t.Name == value {
				return nil
			}
		}
		return errors.New("connector " + c.Name + " has not announced target " + value)
	case store.GrantKindSubnet:
		p := netip.MustParsePrefix(value)
		nets, _ := s.connectors.routes(c.ID, c.Subnets, c.Suffixes)
		for _, a := range nets {
			if ap, err := netip.ParsePrefix(a); err == nil && ap.Bits() <= p.Bits() && ap.Contains(p.Addr()) {
				return nil
			}
		}
		return errors.New("connector " + c.Name + " has not announced a subnet containing " + value)
	}
	_, names := s.connectors.routes(c.ID, c.Subnets, c.Suffixes)
	for _, a := range names {
		if sandbox.UnderSuffix(value, a) {
			return nil
		}
	}
	return errors.New("connector " + c.Name + " has not announced " + value + " or a suffix above it")
}

// wholeReserved refuses a subnet the carve-out would leave empty.
func wholeReserved(kind, value string) error {
	if kind != store.GrantKindSubnet {
		return nil
	}
	p := netip.MustParsePrefix(value)
	if len(sandbox.CarvePlatform([]netip.Prefix{p})) > 0 {
		return nil
	}
	return fmt.Errorf("every address in %s is reserved by the platform (%s), so a sandbox could reach none of it",
		value, strings.Join(sandbox.Shadowed(p), ", "))
}

// resolveGrant checks a request against its connector's announcement and
// returns the grant to store, without a subject. status goes with err.
func (s *Server) resolveGrant(ctx context.Context, st *store.Store, org string, req privateGrantRequest) (store.Grant, store.Connector, int, error) {
	kind, value, err := req.kindValue()
	if err != nil {
		return store.Grant{}, store.Connector{}, http.StatusBadRequest, err
	}
	c, found, err := st.ConnectorByName(ctx, org, req.Connector)
	if err != nil {
		return store.Grant{}, c, http.StatusInternalServerError, err
	}
	if !found {
		return store.Grant{}, c, http.StatusNotFound, errors.New("connector not found")
	}
	if err := s.announced(c, kind, value); err != nil {
		return store.Grant{}, c, http.StatusNotFound, err
	}
	if err := wholeReserved(kind, value); err != nil {
		return store.Grant{}, c, http.StatusBadRequest, err
	}
	return store.Grant{OrgID: org, ConnectorID: c.ID, ConnectorName: c.Name, Kind: kind, Value: value, CreatedAt: s.now().Unix()}, c, 0, nil
}

// grantsLockKey serialises grant checks in one org, so two grants cannot both
// pass the conflict check against a state that has neither.
// ponytail: per replica, like sandboxLocks.
func grantsLockKey(org string) string { return "grants:" + org }

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

// sandboxLocks serializes work on one sandbox. An entry lives only while
// someone holds or waits for it, so the map stays as small as the work in flight.
// ponytail: per replica; two API replicas can still interleave one sandbox's pushes.
type sandboxLocks struct {
	mu sync.Mutex
	m  map[string]*sandboxLock
}

type sandboxLock struct {
	sync.Mutex
	refs int
}

func (l *sandboxLocks) lock(id string) (unlock func()) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sandboxLock{}
	}
	e := l.m[id]
	if e == nil {
		e = &sandboxLock{}
		l.m[id] = e
	}
	e.refs++
	l.mu.Unlock()
	e.Lock()
	return func() {
		e.Unlock()
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.m, id)
		}
		l.mu.Unlock()
	}
}

// pushPrivate stores the sandbox's routed state and sends it to its node.
// status is the node's reply, 0 when the sandbox has no node right now; the
// stored state is applied when it next resumes. Pushes to one sandbox run one
// at a time and each reads the grants afresh, so the last one sent is current.
func (s *Server) pushPrivate(ctx context.Context, st *store.Store, sandboxID, org string) (int, error) {
	defer s.privatePushes.lock(sandboxID)()
	return s.pushPrivateLocked(ctx, st, sandboxID, org)
}

func (s *Server) pushPrivateLocked(ctx context.Context, st *store.Store, sandboxID, org string) (int, error) {
	rec, _, err := st.SandboxByID(ctx, sandboxID)
	if err != nil {
		return 0, err
	}
	gs, err := st.EffectiveGrants(ctx, org, sandboxID, recordTags(rec))
	if err != nil {
		return 0, err
	}
	s.privateConns.closeUngranted(sandboxID, gs)
	cs, err := grantConnectors(ctx, st, gs)
	if err != nil {
		return 0, err
	}
	ts, routes := s.routedState(cs, gs)
	stored, _ := json.Marshal(ts)
	if err := st.SetSandboxPrivate(ctx, sandboxID, string(stored)); err != nil {
		return 0, err
	}
	storedRoutes, _ := json.Marshal(routes)
	if err := st.SetSandboxPrivateRoutes(ctx, sandboxID, string(storedRoutes)); err != nil {
		return 0, err
	}
	node, ok := s.reg.NodeFor(sandboxID)
	if !ok {
		return 0, nil
	}
	patch := map[string]any{"private": ts}
	// An older node ignores a field it does not know and answers 200, so the
	// field goes only where it is understood and its absence is reported below.
	if node.PrivateRoutes {
		patch["private_routes"] = routes
	}
	body, _ := json.Marshal(patch)
	resp, err := s.forward(ctx, node, "PATCH", "/v1/sandboxes/"+sandboxID, body, "application/json", org)
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body read below
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		switch {
		case bytes.Contains(b, []byte("private_routes_unsupported")):
			return http.StatusConflict, errRoutesUnsupported
		case bytes.Contains(b, []byte("private_access_unsupported")):
			return http.StatusConflict, errPrivateUnsupported
		}
		s.log().Warn("node refused a private targets change", "component", "private", "sandbox", sandboxID, "status", resp.StatusCode, "body", string(b))
		return resp.StatusCode, errors.New("the sandbox's node refused the change")
	}
	if !node.PrivateRoutes && !routes.Empty() {
		return http.StatusConflict, errRoutesUnsupported
	}
	return resp.StatusCode, nil
}

// grantPrivate stores one grant on a sandbox and pushes it, rolling it back
// if the push fails. code is empty for plain errors.
func (s *Server) grantPrivate(ctx context.Context, st *store.Store, org, sandboxID string, req privateGrantRequest) (store.Grant, store.Connector, int, string, error) {
	g, c, status, err := s.resolveGrant(ctx, st, org, req)
	if err != nil {
		return g, c, status, "", err
	}
	g.SubjectKind, g.Subject = store.GrantSubjectSandbox, sandboxID
	defer s.privatePushes.lock(grantsLockKey(org))()
	defer s.privatePushes.lock(sandboxID)()
	rec, _, err := st.SandboxByID(ctx, sandboxID)
	if err != nil {
		return g, c, http.StatusInternalServerError, "", err
	}
	gs, err := st.EffectiveGrants(ctx, org, sandboxID, recordTags(rec))
	if err != nil {
		return g, c, http.StatusInternalServerError, "", err
	}
	if status, code, err := s.checkGrants(ctx, st, append(gs, g)); err != nil {
		return g, c, status, code, err
	}
	stored, err := st.AddGrant(ctx, g)
	if errors.Is(err, store.ErrGrantExists) {
		return g, c, http.StatusConflict, "private_grant_exists", errors.New("this sandbox already has that grant")
	}
	if err != nil {
		return g, c, http.StatusInternalServerError, "", err
	}
	status, err = s.pushPrivateLocked(ctx, st, sandboxID, org)
	if errors.Is(err, errRoutesUnsupported) && g.Kind == store.GrantKindTarget {
		// The targets went out; the routes refused are some tag's, not this grant's.
		s.log().Warn("private routes skipped on a node without them", "component", "private", "sandbox", sandboxID)
		err = nil
	}
	if err == nil {
		return stored, c, 0, "", nil
	}
	if rmErr := st.RemoveGrant(ctx, stored); rmErr != nil {
		s.log().Warn("could not roll back a private grant", "component", "private", "sandbox", sandboxID, "grant", stored.ID, "err", rmErr)
	}
	if _, pushErr := s.pushPrivateLocked(ctx, st, sandboxID, org); pushErr != nil {
		s.log().Warn("could not re-push private state after a rollback", "component", "private", "sandbox", sandboxID, "err", pushErr)
	}
	code := "private_push_failed"
	switch {
	case errors.Is(err, errRoutesUnsupported):
		code = "private_routes_unsupported"
	case errors.Is(err, errPrivateUnsupported):
		code = "private_access_unsupported"
	}
	if status == 0 {
		status = http.StatusBadGateway
	}
	return stored, c, status, code, err
}

// grantDetail is the audit text for a grant: phase 1's connector/target for a
// target, connector/kind:value otherwise.
func grantDetail(connector string, g store.Grant) string {
	if g.Kind == store.GrantKindTarget {
		return connector + "/" + g.Value
	}
	return connector + "/" + g.Kind + ":" + g.Value
}

// grantJSON renders one grant; source says where a sandbox's grant comes from.
func (s *Server) grantJSON(g store.Grant, cs map[string]store.Connector) map[string]any {
	out := map[string]any{"id": g.ID, "connector": g.ConnectorName, "kind": g.Kind, "value": g.Value,
		"created_at": g.CreatedAt, "online": s.connectors.count(g.ConnectorID) > 0, "source": "sandbox"}
	if g.SubjectKind == store.GrantSubjectTag {
		out["source"] = "tag:" + g.Subject
	}
	switch g.Kind {
	case store.GrantKindTarget:
		c := cs[g.ConnectorID]
		out["target"], out["names"] = g.Value, privateNames(s.connectors.union(c.ID, c.Targets), g.Value)
	case store.GrantKindSubnet:
		if p, err := netip.ParsePrefix(g.Value); err == nil {
			out["shadowed"] = sandbox.Shadowed(p)
		}
	}
	return out
}

// grantSubjects lists the sandboxes grants reach: their own subjects and every
// member of their tags.
func grantSubjects(ctx context.Context, st *store.Store, org string, gs []store.Grant) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, g := range gs {
		if g.SubjectKind == store.GrantSubjectSandbox {
			add(g.Subject)
			continue
		}
		ids, err := st.SandboxesInTag(ctx, org, g.Subject)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			add(id)
		}
	}
	return out, nil
}

func normalHost(h string) string { return strings.ToLower(strings.TrimSuffix(h, ".")) }

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
	var body privateGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	g, c, status, code, err := s.grantPrivate(r.Context(), st, org, rec.ID, body)
	if err != nil {
		writeGrantErr(w, status, code, err)
		return
	}
	s.auditPrivate(r, st, "private.grant", rec.ID, grantDetail(c.Name, g))
	writeJSON(w, http.StatusCreated, s.grantJSON(g, map[string]store.Connector{c.ID: c}))
}

func (s *Server) handlePrivateGrantList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || privateOff(w) {
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
	gs, err := st.EffectiveGrants(r.Context(), rec.OrgID, rec.ID, recordTags(rec))
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
		out = append(out, s.grantJSON(g, cs))
	}
	node, placed := s.reg.NodeFor(rec.ID)
	writeJSON(w, http.StatusOK, map[string]any{"grants": out,
		"supported": !placed || node.PrivateAccess, "routes_supported": !placed || node.PrivateRoutes})
}

// handlePrivateRevokeID revokes one of the sandbox's own grants by id. A tag's
// grant is revoked on the tag, not here.
func (s *Server) handlePrivateRevokeID(w http.ResponseWriter, r *http.Request) {
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
	g, found, err := st.GrantByID(r.Context(), org, r.PathValue("grant"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found || g.SubjectKind != store.GrantSubjectSandbox || g.Subject != rec.ID {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	}
	s.revokeSandboxGrant(w, r, st, org, rec, g)
}

// handlePrivateRevoke is phase 1's revoke by connector and target name.
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
	g, found, err := st.FindGrant(r.Context(), store.GrantSubjectSandbox, rec.ID, c.ID, store.GrantKindTarget, r.PathValue("target"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	}
	s.revokeSandboxGrant(w, r, st, org, rec, g)
}

// revokeSandboxGrant removes g and re-pushes, which also cuts the sandbox's
// connections nothing else grants.
func (s *Server) revokeSandboxGrant(w http.ResponseWriter, r *http.Request, st *store.Store, org string, rec store.SandboxRecord, g store.Grant) {
	if err := st.RemoveGrant(r.Context(), g); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.pushPrivate(r.Context(), st, rec.ID, org); err != nil {
		// The push may have failed before it cut anything, so cut this grant's connections here.
		if g.Kind == store.GrantKindTarget {
			s.privateConns.closeFor(rec.ID, g.Value)
		}
		s.log().Warn("private revoke not pushed", "component", "private", "sandbox", rec.ID, "err", err)
	}
	s.auditPrivate(r, st, "private.revoke", rec.ID, grantDetail(g.ConnectorName, g))
	w.WriteHeader(http.StatusNoContent)
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

// hasPrivateTargets reports whether a stored private_targets list names any target.
func hasPrivateTargets(raw string) bool { return raw != "" && raw != "[]" }

// hasPrivateRoutes reports whether a stored private_routes value routes anything.
func hasPrivateRoutes(raw string) bool {
	var r sandbox.PrivateRoutes
	return json.Unmarshal([]byte(raw), &r) == nil && !r.Empty()
}

// stripPrivate removes the control-plane-only "private" and "private_routes" keys
// before the body goes to a node. Any spelling: the node's JSON decoding matches
// keys case-insensitively.
func stripPrivate(w http.ResponseWriter, body []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return nil, false
	}
	for k := range fields {
		if strings.EqualFold(k, "private") || strings.EqualFold(k, "private_routes") {
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
		if _, _, status, code, err := s.grantPrivate(ctx, st, org, sandboxID, g); err != nil {
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
	s.privateDNS.forget(sandboxID)
}
