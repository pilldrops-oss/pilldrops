package orchestrator

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

//go:embed install.sh
var installScript []byte

// handleInstallScript serves the node bootstrap script. Public and secret-free;
// the join token it prompts for is the gate.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(installScript)
}

// handleNodeBinary serves a published node binary from NODE_BIN_DIR. Public,
// since install.sh runs before any credential exists.
func (s *Server) handleNodeBinary(w http.ResponseWriter, r *http.Request) {
	dir := os.Getenv("NODE_BIN_DIR")
	name := r.PathValue("name")
	// Only a bare filename is allowed.
	if dir == "" || name == "" || strings.ContainsAny(name, "/\\") || name == ".." {
		writeErr(w, 404, "binary not found")
		return
	}
	http.ServeFile(w, r, filepath.Join(dir, name))
}

// handleMintJoinToken issues a one-time join token and the bootstrap command an
// operator pastes on a new host.
func (s *Server) handleMintJoinToken(w http.ResponseWriter, r *http.Request) {
	// A join token admits a machine of the caller's choosing to the fleet, and
	// that machine is then handed other tenants' sandboxes.
	if !s.requireFleetOperator(w, r) {
		return
	}
	// A node handed no key seals nothing: every sandbox's env would sit in
	// plaintext in a database every node can read.
	if s.nodeToken != "" && s.bootstrapSecret == "" {
		writeErr(w, 503, "orchestrator not configured: neither NODE_BOOTSTRAP_SECRET nor SANDBOXD_SECRET_KEY "+
			"is set (refusing to mint a join token — a node joined now would store sandbox env "+
			"unencrypted. Set SANDBOXD_SECRET_KEY, or NODE_BOOTSTRAP_SECRET to the same value)")
		return
	}
	// A clustered control plane hands its bootstrap DSN to every node that
	// joins; minted now, with none to hand out, the node writes
	// SANDBOXD_DATABASE_URL= (empty) and refuses to start under
	// SANDBOXD_PRODUCTION=1 — found only on the node, long after the operator
	// walked away from this call. Refuse here instead.
	if s.nodeToken != "" && s.bootstrapDSN == "" {
		writeErr(w, 503, "orchestrator not configured: NODE_BOOTSTRAP_DSN is not set "+
			"(refusing to mint a join token — a clustered control plane hands this Postgres DSN to "+
			"every node at join, and a node that joins without one refuses to start under "+
			"SANDBOXD_PRODUCTION=1. Set NODE_BOOTSTRAP_DSN, ideally to the least-privilege role from "+
			"scripts/postgres-node-role.sql over sslmode=require)")
		return
	}
	now := s.now()
	tok := s.join.Mint(now, cluster.JoinTokenTTL)
	orchURL := externalURL(r)
	// The command must not carry the token: it would land in shell history and
	// /proc/<pid>/cmdline. install.sh prompts for it on /dev/tty instead.
	cmd := "curl -fsSL " + orchURL + "/install.sh -o sf-install.sh && " +
		"ORCHESTRATOR_URL=" + orchURL + " sh sf-install.sh"
	writeJSON(w, 200, map[string]any{
		"token":         tok,
		"expires_at":    now.Add(cluster.JoinTokenTTL).Unix(),
		"bootstrap_cmd": cmd,
		"token_hint":    "paste the token when the installer prompts; it is single-use and expires in 30 minutes",
	})
}

// externalURL reconstructs the public base URL from the request, honouring X-Forwarded-Proto.
func externalURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type joinRequest struct {
	Token        string `json:"token"`
	SuggestedID  string `json:"suggested_id"`
	Zone         string `json:"zone"`
	AdvertiseURL string `json:"advertise_url"` // not used at join time; the node re-advertises it via heartbeat
}

// handleJoin redeems a one-time token and returns the node's bootstrap config.
// Not behind s.auth, the token is the authorization. HTTPS-only because it
// hands out the Postgres DSN and secret.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		writeErr(w, 400, "join must be over HTTPS")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // the only pre-auth body
	var body joinRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON body")
		return
	}
	if !s.join.Consume(body.Token, s.now()) {
		writeErr(w, 401, "invalid, expired, or already-used join token")
		return
	}
	zone := body.Zone
	if zone == "" {
		zone = "default"
	}
	taken := make([]string, 0, len(s.reg.Nodes()))
	for _, n := range s.reg.Nodes() {
		taken = append(taken, n.ID)
	}
	if st := s.st.Load(); st != nil {
		if removedIDs, err := st.RemovedNodeIDs(r.Context()); err == nil {
			taken = append(taken, removedIDs...)
		} else {
			s.logger.ErrorContext(r.Context(), "listing removed node ids failed; a re-joined machine "+
				"could be allocated one by coincidence", "component", "orchestrator", "err", err)
		}
	}
	id := s.join.AllocateName(body.SuggestedID, taken)

	writeJSON(w, 200, map[string]any{
		"node_id":          id,
		"node_api_key":     s.nodeKeyFor(id, zone),
		"orchestrator_url": externalURL(r),
		"postgres_dsn":     s.bootstrapDSN,
		"secret_key":       s.bootstrapSecret,
		"zone":             zone,
		"node_token":       s.nodeHeartbeatToken(id, zone), // per-node heartbeat bearer
		// Only the public half: a node verifies org assertions and cannot sign one.
		"org_assert_public_key": orgassert.PublicKeyHex(s.orgAssertKey),
	})
}
