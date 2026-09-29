//go:build !core

package httpapi

import (
	"context"
	"net/http"
	"strings"
)

// The isolation card is measured per deployment rather than hard-coded in the
// frontend. A line that cannot be verified says so instead of claiming either way.

// isolationFact is one line of the card.
type isolationFact struct {
	Label string `json:"label"`
	// State is "yes", "no" or "unknown"; a bool would force unverifiable to false.
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// handleAgentIsolation reports the isolation this workstation actually has.
//
//	GET /v1/sandboxes/{id}/agent/isolation
func (s *Server) handleAgentIsolation(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}

	facts := []isolationFact{s.isolationRuntimeFact(), isolationTLSFact(r), s.isolationKeyFact(r.Context(), id)}
	if mode := s.isolationModeFact(r, id); mode != nil {
		facts = append(facts, *mode)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox_id": id, "facts": facts})
}

// isolationRuntimeFact names the boundary the node enforces.
func (s *Server) isolationRuntimeFact() isolationFact {
	return s.runtimeFactFor(s.d.RuntimeName())
}

// runtimeFactFor maps a runtime name onto the claim the card may make.
func (s *Server) runtimeFactFor(name string) isolationFact {
	switch name {
	case "firecracker":
		return isolationFact{"Hardware-virtualised microVM", "yes",
			"A Firecracker VM with its own kernel. A guest escape has to cross a hypervisor, " +
				"not a namespace."}
	case "docker":
		return isolationFact{"Hardware-virtualised microVM", "no",
			"This node runs the container runtime: isolation is kernel namespaces and cgroups, " +
				"sharing the host kernel. Strong, but not a VM boundary."}
	case "process":
		return isolationFact{"Hardware-virtualised microVM", "no",
			"This node runs sandboxes as local processes — a development mode with no isolation " +
				"boundary. Do not run untrusted code here."}
	case "":
		return isolationFact{"Hardware-virtualised microVM", "unknown",
			"This node did not report a runtime."}
	default:
		// A runtime added later must not inherit a reassuring answer by default.
		return isolationFact{"Hardware-virtualised microVM", "unknown",
			"This node reports the " + name + " runtime, whose isolation this page cannot vouch for."}
	}
}

// isolationTLSFact reports whether this request was encrypted, measured from
// the request so an unencrypted hop behind a TLS edge is not hidden.
func isolationTLSFact(r *http.Request) isolationFact {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" && r.TLS != nil {
		proto = "https"
	}
	switch {
	case strings.EqualFold(proto, "https"):
		return isolationFact{"Encrypted in transit", "yes",
			"This session is over HTTPS, including the event stream."}
	case proto == "":
		return isolationFact{"Encrypted in transit", "unknown",
			"This request arrived without TLS and without a proxy protocol header, so the " +
				"hop in front of this node cannot be confirmed either way."}
	default:
		return isolationFact{"Encrypted in transit", "no",
			"This request arrived over " + proto + ". Anything sent on it, including tasks and " +
				"tokens, is readable in transit."}
	}
}

// isolationKeyFact reports whether a provider key is stored: no for the
// per-request BYOK path, yes for the config path.
func (s *Server) isolationKeyFact(ctx context.Context, id string) isolationFact {
	cfg := s.st.SandboxConfigFor(ctx, id)
	stored := []string{}
	for name := range cfg.Env {
		if secretish(name) {
			stored = append(stored, name)
		}
	}
	if len(stored) == 0 {
		return isolationFact{"Provider key not stored", "yes",
			"No credential is stored for this workstation. Keys supplied per request are used " +
				"for one upstream call and never written to disk."}
	}
	return isolationFact{"Provider key not stored", "no",
		"This workstation has a stored credential (" + strings.Join(stored, ", ") + ") so it " +
			"survives a restart and can be rotated without rebuilding. It is encrypted at rest " +
			"and removed when the workstation is destroyed."}
}

// isolationModeFact reports the autonomy agentd is actually running in, which
// can differ from what the launcher requested. Nil when the agent is unreachable.
func (s *Server) isolationModeFact(r *http.Request, id string) *isolationFact {
	c, err := s.agentClient(id)
	if err != nil {
		return nil
	}
	st, err := c.Status(r.Context())
	if err != nil {
		return nil
	}
	mode := st.Autonomy.Default
	if st.CurrentTurn != nil && st.CurrentTurn.Mode != "" {
		mode = st.CurrentTurn.Mode
	}
	switch mode {
	case "supervised":
		return &isolationFact{"Autonomy: supervised", "yes",
			"Every tool call waits for you."}
	case "guided":
		return &isolationFact{"Autonomy: guided", "yes",
			"Reads and searches run freely; anything outside the allowlist waits for your approval."}
	case "autonomous":
		return &isolationFact{"Autonomy: autonomous", "no",
			"The agent acts without asking, including writes and commands. Nothing here reviews " +
				"what it does before it happens."}
	case "":
		return nil
	default:
		return &isolationFact{"Autonomy: " + mode, "unknown",
			"This mode is not one this page knows how to describe."}
	}
}
