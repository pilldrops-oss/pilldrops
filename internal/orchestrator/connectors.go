package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	maxConnectorsPerOrg   = 10
	maxConnectorInstances = 5
	connectorLogLimit     = 200
)

func privateIngressEnabled() bool {
	switch strings.ToLower(os.Getenv("PRIVATE_INGRESS")) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func privateAccessOn() bool {
	return privateIngressEnabled() || strings.EqualFold(os.Getenv("PRIVATE_INGRESS"), "remote")
}

func (s *Server) registerConnectorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/connectors", s.auth(s.handleConnectorCreate))
	mux.HandleFunc("GET /v1/connectors", s.auth(s.handleConnectorList))
	mux.HandleFunc("DELETE /v1/connectors/{id}", s.auth(s.handleConnectorRevoke))
	mux.HandleFunc("GET /v1/connectors/{id}/connections", s.auth(s.handleConnectorLog))
	mux.HandleFunc("POST /v1/connectors/{id}/keys", s.auth(s.handleConnectorKeyCreate))
	mux.HandleFunc("GET /v1/connectors/{id}/keys", s.auth(s.handleConnectorKeyList))
	mux.HandleFunc("DELETE /v1/connectors/{id}/keys/{key}", s.auth(s.handleConnectorKeyRevoke))
	// A pd_conn_ key is not an API key, so this route authenticates itself.
	mux.HandleFunc("POST /v1/connectors/session", s.handleConnectorSession)
	mux.HandleFunc("GET /v1/connectors/{id}/tunnel", s.handleConnectorControl)
	mux.HandleFunc("GET /v1/connector-conns/{conn}", s.handleConnectorConn)
}

// privateAdmin is the one gate for changing connectors and grants.
func (s *Server) privateAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	if _, role := callerKey(r.Context()); role == principalSandboxRole {
		writeErr(w, http.StatusForbidden, "a sandbox cannot manage private access")
		return "", false
	}
	return fastOrg(r), true
}

func privateOff(w http.ResponseWriter) bool {
	if privateAccessOn() {
		return false
	}
	writeJSONErr(w, http.StatusNotImplemented, "no_private_ingress",
		"private access needs one orchestrator replica started with PRIVATE_INGRESS=1 (and PRIVATE_INGRESS=remote on the API-only replicas)")
	return true
}

func validConnectorHost(host string, platform []string) error {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if net.ParseIP(h) != nil {
		return nil
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || !strings.Contains(h, ".") {
		return fmt.Errorf("host %q is not allowed", host)
	}
	for _, p := range platform {
		p = strings.ToLower(p)
		if p != "" && (h == p || strings.HasSuffix(h, "."+p)) {
			return fmt.Errorf("host %q is under this platform's own domain", host)
		}
	}
	if len(h) > 253 {
		return fmt.Errorf("host %q is too long", host)
	}
	for _, label := range strings.Split(h, ".") {
		if !sandbox.ValidPrivateName(label) {
			return fmt.Errorf("host %q is not a valid hostname", host)
		}
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(h); err != nil {
		return fmt.Errorf("host %q is a public suffix", host)
	}
	return nil
}

// platformDomains lists the hosts a connector may never shadow inside a sandbox.
func platformDomains() []string {
	var out []string
	for _, k := range []string{"PORTAL_ORIGIN", "PORTAL_SANDBOX_DOMAIN", "SHARE_DOMAIN"} {
		v := os.Getenv(k)
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			v = u.Hostname()
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func newConnectorID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cn_" + hex.EncodeToString(b), nil
}

func (s *Server) connectorTokenKey() []byte { return previewtoken.BrowserKey("connector:" + s.apiKey) }

func (s *Server) mintConnectorToken(id string) string {
	return previewtoken.BrowserAt(s.connectorTokenKey(), id, 1, "tunnel", s.now().Add(shareTokenTTL))
}

func (s *Server) validConnectorToken(tok, id string) bool {
	if s.apiKey == "" && !devInsecure() {
		return false
	}
	return previewtoken.BrowserValidAt(s.connectorTokenKey(), tok, id, 1, "tunnel", s.now())
}

func connectorJSON(c store.Connector, online int) map[string]any {
	ts := c.Targets
	if ts == nil {
		ts = []store.ConnectorTarget{}
	}
	return map[string]any{"id": c.ID, "name": c.Name, "created_at": c.CreatedAt, "last_seen_at": c.LastSeenAt,
		"instances": online, "online": online > 0, "targets": ts}
}

func (s *Server) auditPrivate(r *http.Request, st *store.Store, action, target, detail string) {
	keyID, _ := callerKey(r.Context())
	if err := st.RecordAuditDetail(r.Context(), fastOrg(r), keyID, action, target, detail); err != nil {
		s.log().Warn("audit write failed", "component", "private", "action", action, "err", err)
	}
}

func (s *Server) handleConnectorCreate(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || privateOff(w) {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	c, status, err := s.createConnector(r, st, org, body.Name)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, connectorJSON(c, 0))
}

func (s *Server) createConnector(r *http.Request, st *store.Store, org, name string) (store.Connector, int, error) {
	if !sandbox.ValidPrivateName(name) {
		return store.Connector{}, http.StatusBadRequest, errors.New(`"name" must be a lowercase DNS label`)
	}
	n, err := st.CountConnectors(r.Context(), org)
	if err != nil {
		return store.Connector{}, http.StatusInternalServerError, err
	}
	if n >= maxConnectorsPerOrg {
		return store.Connector{}, http.StatusTooManyRequests, fmt.Errorf("this org already has %d connectors, the limit; remove one first", n)
	}
	id, err := newConnectorID()
	if err != nil {
		return store.Connector{}, http.StatusInternalServerError, err
	}
	keyID, _ := callerKey(r.Context())
	c := store.Connector{ID: id, Name: name, OrgID: org, CreatedBy: keyID, CreatedAt: s.now().Unix()}
	if err := st.InsertConnector(r.Context(), c); errors.Is(err, store.ErrConnectorNameTaken) {
		return store.Connector{}, http.StatusConflict, err
	} else if err != nil {
		return store.Connector{}, http.StatusInternalServerError, err
	}
	s.auditPrivate(r, st, "connector.create", id, name)
	return c, 0, nil
}

func (s *Server) handleConnectorList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if _, role := callerKey(r.Context()); role == principalSandboxRole {
		writeErr(w, http.StatusForbidden, "a sandbox cannot list connectors")
		return
	}
	cs, err := st.ListConnectors(r.Context(), fastOrg(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		c.Targets = s.connectors.union(c.ID, c.Targets)
		out = append(out, connectorJSON(c, s.connectors.count(c.ID)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": out})
}

func (s *Server) ownConnector(w http.ResponseWriter, r *http.Request, st *store.Store, org string) (store.Connector, bool) {
	c, ok, err := st.ConnectorByID(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return store.Connector{}, false
	}
	if !ok || c.RevokedAt != 0 {
		writeErr(w, http.StatusNotFound, "connector not found")
		return store.Connector{}, false
	}
	return c, true
}

func (s *Server) handleConnectorRevoke(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	c, ok := s.ownConnector(w, r, st, org)
	if !ok {
		return
	}
	granted, err := st.SandboxesGrantedConnector(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := st.RevokeConnector(r.Context(), org, c.ID, s.now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.connectors.closeConnector(c.ID, "revoked")
	for _, sandboxID := range granted {
		if _, err := s.pushPrivate(r.Context(), st, sandboxID, org); err != nil {
			s.log().Warn("revoked connector not pushed", "component", "private", "sandbox", sandboxID, "err", err)
		}
	}
	s.auditPrivate(r, st, "connector.revoke", c.ID, c.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConnectorLog(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if _, role := callerKey(r.Context()); role == principalSandboxRole {
		writeErr(w, http.StatusForbidden, "a sandbox cannot read the connection log")
		return
	}
	c, ok := s.ownConnector(w, r, st, fastOrg(r))
	if !ok {
		return
	}
	rows, err := st.PrivateConns(r.Context(), c.ID, connectorLogLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, map[string]any{"id": p.ID, "sandbox_id": p.SandboxID, "target": p.Target, "started_at": p.StartedAt,
			"ended_at": p.EndedAt, "bytes_in": p.BytesIn, "bytes_out": p.BytesOut, "result": p.Result})
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (s *Server) handleConnectorKeyCreate(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	c, ok := s.ownConnector(w, r, st, org)
	if !ok {
		return
	}
	k, plain, err := st.CreateConnectorKey(r.Context(), c.ID, s.now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditPrivate(r, st, "connector.key.create", c.ID, k.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": k.ID, "key": plain, "created_at": k.CreatedAt})
}

func (s *Server) handleConnectorKeyList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	c, ok := s.ownConnector(w, r, st, org)
	if !ok {
		return
	}
	ks, err := st.ListConnectorKeys(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(ks))
	for _, k := range ks {
		out = append(out, map[string]any{"id": k.ID, "created_at": k.CreatedAt, "last_used_at": k.LastUsedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleConnectorKeyRevoke(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	org, ok := s.privateAdmin(w, r)
	if !ok {
		return
	}
	c, ok := s.ownConnector(w, r, st, org)
	if !ok {
		return
	}
	done, err := st.RevokeConnectorKey(r.Context(), c.ID, r.PathValue("key"), s.now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !done {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	s.auditPrivate(r, st, "connector.key.revoke", c.ID, r.PathValue("key"))
	w.WriteHeader(http.StatusNoContent)
}

// handleConnectorSession returns a tunnel token for a connector, creating it
// first when an admin's API key names one that does not exist yet.
func (s *Server) handleConnectorSession(w http.ResponseWriter, r *http.Request) {
	if privateOff(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	tok := bearer(r)
	if strings.HasPrefix(tok, "pd_conn_") {
		st := s.st.Load()
		if st == nil {
			writeErr(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
		c, ok, err := st.ConnectorForKey(r.Context(), tok, s.now().Unix())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "connector key is invalid or revoked")
			return
		}
		if body.Name != "" && body.Name != c.Name {
			writeErr(w, http.StatusForbidden, "this key belongs to connector "+c.Name)
			return
		}
		writeJSON(w, http.StatusOK, s.sessionJSON(r, c))
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	s.auth(func(w http.ResponseWriter, r *http.Request) {
		st := s.projectStore(w)
		if st == nil {
			return
		}
		org, ok := s.privateAdmin(w, r)
		if !ok {
			return
		}
		c, found, err := st.ConnectorByName(r.Context(), org, body.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			var status int
			if c, status, err = s.createConnector(r, st, org, body.Name); err != nil {
				writeErr(w, status, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, s.sessionJSON(r, c))
	})(w, r)
}

func (s *Server) sessionJSON(r *http.Request, c store.Connector) map[string]any {
	scheme := "wss"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "ws"
	}
	base := scheme + "://" + r.Host + "/v1/connectors/"
	return map[string]any{"id": c.ID, "name": c.Name, "tunnel_url": base + c.ID + "/tunnel",
		"conn_url": scheme + "://" + r.Host + "/v1/connector-conns/", "tunnel_token": s.mintConnectorToken(c.ID)}
}
