package orchestrator

// Port forwards publish a guest port on a raw public TCP/UDP port. This file
// owns the API and the row; portingress.go owns the listener that terminates
// the public port. Named portforward.go, not forwards.go, because forward.go
// beside it is the generic node relay and the two are not related.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// forwardRangeDefault is the public port range boxd-style endpoints use.
const forwardRangeLo, forwardRangeHi = 40000, 60000

// forwardRange reads FORWARD_PORT_RANGE ("lo-hi"). An unparseable value falls
// back to the default range rather than refusing: the range is an operator
// convenience, not a security boundary.
func forwardRange() (lo, hi int) {
	raw := strings.TrimSpace(os.Getenv("FORWARD_PORT_RANGE"))
	l, h, ok := strings.Cut(raw, "-")
	if !ok {
		return forwardRangeLo, forwardRangeHi
	}
	lo, loErr := strconv.Atoi(strings.TrimSpace(l))
	hi, hiErr := strconv.Atoi(strings.TrimSpace(h))
	if loErr != nil || hiErr != nil || lo < 1 || hi < lo || hi > 65535 {
		return forwardRangeLo, forwardRangeHi
	}
	return lo, hi
}

// forwardPublicHost is the hostname a forward endpoint is published under:
// FORWARD_PUBLIC_HOST, else the sandbox domain. Empty means the deployment has
// no public name for its ingress and forwards are refused.
func (s *Server) forwardPublicHost() string {
	if h := strings.TrimSpace(os.Getenv("FORWARD_PUBLIC_HOST")); h != "" {
		return h
	}
	return s.sandboxDomain
}

// forwardEndpoint renders one row's public address.
func (s *Server) forwardEndpoint(f store.PortForward) string {
	return s.forwardPublicHost() + ":" + strconv.Itoa(f.PublicPort)
}

func (s *Server) forwardJSON(f store.PortForward) map[string]any {
	protos := make([]string, 0, 2)
	if f.TCP {
		protos = append(protos, "tcp")
	}
	if f.UDP {
		protos = append(protos, "udp")
	}
	return map[string]any{
		"guest_port": f.GuestPort, "public_port": f.PublicPort,
		"tcp": f.TCP, "udp": f.UDP, "protocols": protos,
		"endpoint": s.forwardEndpoint(f), "created_at": f.CreatedAt,
	}
}

// registerForwardRoutes mounts /v1/sandboxes/{id}/forwards. Registered
// explicitly so the {id}/{rest...} catch-all does not relay them to a node:
// the row lives in this tier's store, not on the node.
func (s *Server) registerForwardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sandboxes/{id}/forwards", s.auth(s.bySlug(s.handleForwardCreate)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/forwards", s.auth(s.bySlug(s.handleForwardList)))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/forwards/{port}", s.auth(s.bySlug(s.handleForwardDelete)))
}

// forwardSandbox resolves the path id and checks ownership, answering 404 for
// anything the caller may not act on (never 403, which would confirm the id).
func (s *Server) forwardSandbox(w http.ResponseWriter, r *http.Request) (*store.Store, string, bool) {
	st := s.projectStore(w)
	if st == nil {
		return nil, "", false
	}
	id := r.PathValue("id")
	if !s.callerOwnsSandbox(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return nil, "", false
	}
	return st, id, true
}

func (s *Server) handleForwardCreate(w http.ResponseWriter, r *http.Request) {
	st, id, ok := s.forwardSandbox(w, r)
	if !ok {
		return
	}
	if s.forwardPublicHost() == "" {
		writeJSONErr(w, http.StatusNotImplemented, "no_forward_host",
			"port forwarding needs FORWARD_PUBLIC_HOST (or PORTAL_SANDBOX_DOMAIN) set on the "+
				"orchestrator: without it there is no address to publish a port on")
		return
	}
	// A row is not a listener. Nothing binds the public port unless some
	// replica runs the ingress, and with FORWARD_INGRESS unset everywhere —
	// the default — this used to answer 201 with an endpoint that refuses
	// every connection and tell the caller nothing.
	if !forwardIngressDeclared() {
		writeJSONErr(w, http.StatusNotImplemented, "no_forward_ingress",
			"no replica of this deployment binds public forward ports, so a forward would be a row "+
				"with an address nothing answers on. Start one orchestrator replica with "+
				"FORWARD_INGRESS=1 (and FORWARD_INGRESS=remote on the API-only replicas, which is how "+
				"they know the ingress exists)")
		return
	}
	var body struct {
		Port int   `json:"port"`
		TCP  *bool `json:"tcp"`
		UDP  *bool `json:"udp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Port < 1 || body.Port > 65535 {
		writeErr(w, http.StatusBadRequest, `"port" must be 1-65535`)
		return
	}
	f := store.PortForward{
		SandboxID: id, GuestPort: body.Port,
		TCP: body.TCP == nil || *body.TCP, UDP: body.UDP != nil && *body.UDP,
	}
	if !f.TCP && !f.UDP {
		writeErr(w, http.StatusBadRequest, "a forward must carry at least one of tcp or udp")
		return
	}
	if !s.forwardPortServable(w, r, st, id, f) {
		return
	}
	if !s.forwardPublishable(w, r, st, id) {
		return
	}

	existing, err := st.GetPortForward(r.Context(), id, body.Port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The cap counts distinct guest ports, so re-exposing one already published
	// is never refused — otherwise changing a protocol would need a delete first.
	if existing.PublicPort == 0 && !s.forwardWithinPlan(w, r, st, id) {
		return
	}

	f.CreatedAt = s.now().Unix()
	lo, hi := forwardRange()
	row, err := st.UpsertPortForward(r.Context(), f, lo, hi)
	if err != nil {
		if errors.Is(err, store.ErrNoPublicPort) {
			writeJSONErr(w, http.StatusServiceUnavailable, "no_public_port", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reconcileForwards(r.Context())
	status := http.StatusCreated
	if existing.PublicPort != 0 {
		status = http.StatusOK
	}
	writeJSON(w, status, s.forwardJSON(row))
}

// forwardPublishable refuses to publish a sandbox nobody may reach without a
// credential. The ingress applies the same rule before it splices a connection
// (portingress.go); this one exists so the caller is told at the API instead of
// being handed a public port that quietly serves nothing.
func (s *Server) forwardPublishable(w http.ResponseWriter, r *http.Request, st *store.Store, id string) bool {
	row, found, err := st.GetActiveSandboxRow(r.Context(), id)
	if err != nil || !found {
		writeErr(w, http.StatusServiceUnavailable, "cannot read sandbox "+id+" to judge whether it may be published")
		return false
	}
	if !sandbox.ReachableAnonymously(row.Visibility, row.CreatedBy) {
		writeJSONErr(w, http.StatusConflict, "sandbox_private",
			fmt.Sprintf("sandbox %s is private, and a forward publishes a raw port that carries no credential to "+
				"check — share it with the org (POST /v1/sandboxes/%s/visibility {\"visibility\":\"org\"}) or reach "+
				"it through its preview URL, which takes a token", id, id))
		return false
	}
	return true
}

// forwardWithinPlan enforces MaxForwards. Written like the workstation check in
// admission.go: a plain count, refused with the plan id and the number in it.
func (s *Server) forwardWithinPlan(w http.ResponseWriter, r *http.Request, st *store.Store, id string) bool {
	org := fastOrg(r)
	if org == "" || org == "default" {
		return true // operator/unscoped calls are not a customer's quota
	}
	a, found, err := st.PlanFor(r.Context(), org)
	if err != nil || !found {
		// Self-hosted fails open, multi-tenant closed, same rule as admitDecide.
		if s.multiTenant {
			writeErr(w, http.StatusServiceUnavailable, admissionUnavailable)
			return false
		}
		return true
	}
	plan, ok := entitlements.PlanByID(a.PlanID)
	if !ok {
		if s.multiTenant {
			writeErr(w, http.StatusServiceUnavailable, admissionUnavailable)
			return false
		}
		return true
	}
	n, err := st.CountPortForwards(r.Context(), id)
	if err != nil {
		if s.multiTenant {
			writeErr(w, http.StatusServiceUnavailable, admissionUnavailable)
			return false
		}
		return true
	}
	if n >= plan.Forwards() {
		writeJSONErr(w, http.StatusConflict, "forward_limit",
			fmt.Sprintf("plan '%s' allows %d forwarded port(s) per sandbox and this sandbox already has %d "+
				"— remove one or upgrade", plan.ID, plan.Forwards(), n))
		return false
	}
	return true
}

// forwardPortServable refuses a port the owning runtime cannot deliver to.
// Docker publishes the app port plus the template's declared ports at create
// and nothing else, so a forward to any other port would be a listener that
// can never connect.
func (s *Server) forwardPortServable(w http.ResponseWriter, r *http.Request, st *store.Store, id string, f store.PortForward) bool {
	node, ok := s.reg.NodeFor(id)
	if !ok || node.Runtime != "docker" {
		return true
	}
	// Docker publishes a container port as TCP only (`-p 127.0.0.1:0:<p>`,
	// boot.go), so a UDP forward would be a public socket with nothing behind
	// it. Refused rather than accepted and silently dropped.
	if f.UDP {
		writeJSONErr(w, http.StatusConflict, "udp_not_supported",
			fmt.Sprintf("sandbox %s runs on the docker runtime, which publishes container ports as TCP "+
				"only, so a UDP forward could never reach the guest; place it on a firecracker node "+
				"for UDP", id))
		return false
	}
	port := f.GuestPort
	row, found, err := st.GetActiveSandboxRow(r.Context(), id)
	if err != nil || !found {
		return true // no row to judge by; the node refuses the tunnel if it cannot serve it
	}
	declared := []int{row.PublicPort}
	if t, ok := templates.Resolve(row.Template); ok {
		declared = append(declared, t.Ports...)
	}
	if slices.Contains(declared, port) {
		return true
	}
	writeJSONErr(w, http.StatusConflict, "port_not_declared",
		fmt.Sprintf("sandbox %s runs on the docker runtime, which publishes only the ports declared when the "+
			"sandbox was created (%s); port %d has nothing behind it and cannot be forwarded",
			id, joinPorts(declared), port))
	return false
}

func joinPorts(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		if p > 0 {
			out = append(out, strconv.Itoa(p))
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

func (s *Server) handleForwardList(w http.ResponseWriter, r *http.Request) {
	st, id, ok := s.forwardSandbox(w, r)
	if !ok {
		return
	}
	rows, err := st.PortForwardsBySandbox(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, f := range rows {
		out = append(out, s.forwardJSON(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"forwards": out})
}

func (s *Server) handleForwardDelete(w http.ResponseWriter, r *http.Request) {
	st, id, ok := s.forwardSandbox(w, r)
	if !ok {
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "guest port must be a number")
		return
	}
	removed, err := st.DeletePortForward(r.Context(), id, port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "no forward for that port")
		return
	}
	s.reconcileForwards(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// forwardsFor is the sandbox-detail projection, so GET /v1/sandboxes/{id}
// carries the endpoints without a second call. Never errors the caller's read.
func (s *Server) forwardsFor(r *http.Request, id string) []map[string]any {
	st := s.st.Load()
	if st == nil {
		return nil
	}
	rows, err := st.PortForwardsBySandbox(r.Context(), id)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, f := range rows {
		out = append(out, s.forwardJSON(f))
	}
	return out
}
