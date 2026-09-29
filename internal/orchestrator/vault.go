package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The secrets vault holds org-scoped named secrets, resolved into a sandbox
// create by name so a provider key never travels through a browser twice.

// registerVaultRoutes mounts the /v1/secrets family. No store check here: the
// store opens in a background goroutine and is nil when Routes() runs, so the
// nil case is handled per request by projectStore.
func (s *Server) registerVaultRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/secrets", s.auth(s.handleSecretList))
	mux.HandleFunc("POST /v1/secrets", s.auth(s.handleSecretPut))
	mux.HandleFunc("DELETE /v1/secrets/{name}", s.auth(s.handleSecretDelete))
	mux.HandleFunc("POST /v1/secrets/{name}/scope", s.auth(s.handleSecretScope))
}

func (s *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	list, err := st.ListSecrets(r.Context(), fastOrg(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSecretPut(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	// secret defaults to true so an old client that never sends it keeps sealing.
	var body struct {
		Name       string `json:"name"`
		Value      string `json:"value"`
		Secret     *bool  `json:"secret"`
		AutoInject bool   `json:"auto_inject"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	callerID, _ := callerKey(r.Context())
	got, err := st.PutSecret(r.Context(), fastOrg(r), body.Name, body.Value,
		body.Secret == nil || *body.Secret, body.AutoInject, callerID)
	if err != nil {
		if errors.Is(err, store.ErrVaultName) || err.Error() == "value is required" {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// handleSecretScope changes which sandboxes a secret may reach. The value is
// preserved: this is not a rotation, and a caller must never have to re-type a
// credential to narrow where it goes.
func (s *Server) handleSecretScope(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	scope, err := store.ValidScope(body.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.PathValue("name")
	ok, err := st.SetSecretScope(r.Context(), fastOrg(r), name, scope)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "scope": scope})
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	deleted, err := st.DeleteSecret(r.Context(), fastOrg(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scopeAllows decides whether a secret may be injected into the sandbox being
// created. Refusals name the secret: a create that silently dropped it would
// hand back a sandbox whose app is missing a credential it asked for.
//
// "all" is the default and behaves as before scopes existed. "private" reaches
// only its own creator's private sandboxes — a sandbox the org can see would
// hand the secret to the org. "shared" is the opposite: a credential meant for
// team machines, kept out of one person's private one.
func scopeAllows(name, scope, owner, visibility, callerID string) error {
	switch scope {
	case store.ScopePrivate:
		if visibility != sandbox.VisibilityPrivate {
			return fmt.Errorf("secret %q is scoped private: it can only be injected into a private sandbox "+
				"(create with visibility \"private\", or change the secret's scope)", name)
		}
		if owner == "" || owner != callerID {
			return fmt.Errorf("secret %q is scoped private to the key that stored it, which is not this one", name)
		}
	case store.ScopeShared:
		if visibility == sandbox.VisibilityPrivate {
			return fmt.Errorf("secret %q is scoped shared: it is for sandboxes the org can see, not a private one", name)
		}
	}
	return nil
}

// resolveSecretRefs rewrites a create body's secret_refs into env entries from
// the caller's vault, appends the org's auto_inject entries, and strips the
// field; nodes only ever see ordinary env. Explicit env and refs win over
// auto_inject on a name collision. An unresolvable ref fails the create; an
// unrecognised body passes through unchanged.
func (s *Server) resolveSecretRefs(r *http.Request, raw []byte) ([]byte, error) {
	st := s.st.Load()
	if st == nil || len(raw) == 0 {
		return raw, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, nil
	}
	org := fastOrg(r)
	refs, _ := obj["secret_refs"].([]any)
	auto, err := st.ListAutoInject(r.Context(), org)
	if err != nil {
		return nil, fmt.Errorf("auto-inject vault entries could not be read: %w", err)
	}
	if len(refs) == 0 && len(auto) == 0 {
		return raw, nil
	}
	env, _ := obj["env"].([]any)
	have := map[string]bool{}
	for _, e := range env {
		if kv, ok := e.(string); ok {
			name, _, _ := strings.Cut(kv, "=")
			have[name] = true
		}
	}
	// What the sandbox being created will be: the request's visibility (empty
	// means the platform default, org) and the key asking for it.
	wantVisibility, _ := obj["visibility"].(string)
	if wantVisibility == "" {
		wantVisibility = sandbox.VisibilityOrg
	}
	callerID, _ := callerKey(r.Context())
	for _, ra := range refs {
		name, _ := ra.(string)
		val, scope, owner, found, err := st.SecretForInject(r.Context(), org, name)
		if err != nil {
			return nil, fmt.Errorf("secret %q could not be read: %w", name, err)
		}
		if !found {
			return nil, fmt.Errorf("secret %q is not in your vault — connect it under Vault first", name)
		}
		if err := scopeAllows(name, scope, owner, wantVisibility, callerID); err != nil {
			return nil, err
		}
		env = append(env, name+"="+val)
		have[name] = true
	}
	names := make([]string, 0, len(auto))
	for name := range auto {
		if !have[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+auto[name])
	}
	obj["env"] = env
	delete(obj, "secret_refs")
	return json.Marshal(obj)
}
