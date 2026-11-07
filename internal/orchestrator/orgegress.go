package orchestrator

import (
	"encoding/json"
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// An org may carry a base egress policy every create in it starts from; a
// request may only narrow it. The store has held one since the column landed
// and nothing could write it, so the base half of the three-way resolve was
// unreachable by any route. Operator-only, like preview sources: it is a
// containment control, and a tenant that could set its own would set it wider.

func (s *Server) registerOrgEgressRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/orgs/{org}/egress", s.auth(s.handleOrgEgressGet))
	mux.HandleFunc("PUT /v1/admin/orgs/{org}/egress", s.auth(s.handleOrgEgressPut))
}

func (s *Server) handleOrgEgressGet(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || !s.requireEgressOperator(w, r) {
		return
	}
	rules, ok, err := st.OrgEgress(r.Context(), r.PathValue("org"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"org": r.PathValue("org"), "set": ok}
	if ok {
		out["network"] = rules
		// The same advisories a create reports, so an operator setting a
		// deny-only base learns here that it denies everything.
		if adv := rules.Advisories(); len(adv) > 0 {
			out["network_advisories"] = adv
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleOrgEgressPut(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || !s.requireEgressOperator(w, r) {
		return
	}
	// A null network clears the override; templates' own defaults apply again.
	var body struct {
		Network *egress.Rules `json:"network"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Network != nil {
		if err := body.Network.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, "network: "+err.Error())
			return
		}
		if err := egress.CheckWildcards(body.Network); err != nil {
			writeErr(w, http.StatusBadRequest, "network: "+err.Error())
			return
		}
		// Same refusal as a create carrying domains, and for a worse reason: a
		// stored domain rule would fail every later create in the org at boot,
		// far from whoever typed it.
		if body.Network.HasDomains() && !s.reg.AnyWithCapability(cluster.CapEgressDomains, s.now(), cluster.NodeTTL) {
			writeJSON(w, http.StatusNotImplemented, map[string]string{
				"error": "domain egress rules need the name layer that reads TLS SNI and the " +
					"HTTP Host header, and no online node runs it; address and CIDR rules are " +
					"enforced and work today",
				"code": "egress_not_enforced",
			})
			return
		}
	}
	if err := st.SetOrgEgress(r.Context(), r.PathValue("org"), body.Network); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"org": r.PathValue("org"), "set": body.Network != nil, "network": body.Network}
	if adv := body.Network.Advisories(); len(adv) > 0 {
		out["network_advisories"] = adv
	}
	writeJSON(w, http.StatusOK, out)
}

// requireEgressOperator answers 403 and returns false for a customer credential.
func (s *Server) requireEgressOperator(w http.ResponseWriter, r *http.Request) bool {
	if isOperator(r) {
		return true
	}
	writeErr(w, http.StatusForbidden, "an org's base egress policy is an operator setting")
	return false
}
