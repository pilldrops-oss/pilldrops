package httpapi

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/hoststats"
)

// SetCluster wires this node to its orchestrator. Empty orchURL leaves the node standalone.
func (s *Server) SetCluster(orchURL, orchKey string) {
	s.orchURL = orchURL
	s.orchKey = orchKey
}

// Clustered reports whether this node is part of a multi-node cluster.
func (s *Server) Clustered() bool { return s.orchURL != "" }

// SetOrgOverrideSecret wires the HMAC secret for X-Org-Override assertions.
// Empty leaves overrides refused unless SANDBOXD_DEV=1/DEV_INSECURE=1.
func (s *Server) SetOrgOverrideSecret(secret []byte) { s.orgOverrideSecret = secret }

// SetNodeID sets the id org assertions must be minted for. Must equal the
// heartbeat's nodeboot.NodeID or every override fails closed with a 403.
func (s *Server) SetNodeID(id string) { s.nodeID = id }

// SetOrgAssertPublicKey wires the control plane's Ed25519 verifying key. When
// set, the HMAC form is no longer accepted: an HMAC secret lets any node mint
// assertions for any tenant, a public key mints nothing.
func (s *Server) SetOrgAssertPublicKey(pub ed25519.PublicKey) { s.orgAssertPub = pub }

// SetPreviewSecret wires the secret for /preview/ tokens. Empty fails closed
// (every /preview/ request refused) unless SANDBOXD_DEV=1/DEV_INSECURE=1.
func (s *Server) SetPreviewSecret(secret []byte) { s.previewSecret = secret }

// handleNodes proxies the orchestrator's /v1/nodes, or synthesizes a single
// node from this host when standalone.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if s.orchURL == "" {
		st := hoststats.Sample(s.dataDir)
		vcpu, memMB := 0, 0
		for _, b := range s.d.List("") {
			vcpu += b.VCPU
			memMB += b.MemMiB
		}
		node := map[string]any{
			"id": "local", "url": "", "healthy": true, "last_seen": time.Now().Unix(),
			"capacity": map[string]any{
				"cpu_cores": st.Cores, "cpu_percent": st.CPUPercent, "load1": st.LoadAvg[0],
				"mem_total_mb": st.MemTotalMB, "mem_available_mb": st.MemAvailMB,
				"mem_committed_mb": memMB, "disk_free_gb": st.DiskFreeGB,
				"sandboxes_active": len(s.d.List("")),
			},
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": []any{node}})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", s.orchURL+"/v1/nodes", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if s.orchKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.orchKey)
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "orchestrator unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // response already committed
}
