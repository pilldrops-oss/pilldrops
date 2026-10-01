package main

// Secrets vault: org-scoped named secrets stored AES-256-GCM encrypted at rest
// and injected into sandbox creates via `secret_refs`, so a raw key never
// appears in a create body, CLI snippet, or log. Unlike API keys (a hash,
// verify-only), vault entries store a ciphertext that must be recoverable.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// VaultSecret is one stored secret. Name doubles as the env var the secret
// becomes at create time, so it must be a valid env identifier.
type VaultSecret struct {
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`
	// Kind is "secret" (sealed; List returns Last4 only) or "env" (List returns Value).
	Kind string `json:"kind"`
	// AutoInject adds the entry to every sandbox the owner creates.
	AutoInject bool   `json:"auto_inject"`
	Cipher     string `json:"cipher,omitempty"` // base64(nonce || AES-GCM(value)); never sent to clients
	// Last4 lets the UI say "sk-…abcd connected" without ever exposing the key.
	Last4 string `json:"last4"`
	// Value is set only for kind "env", never for credentials.
	Value     string `json:"value,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// legacyReadable seeds Kind for entries stored before kind existed: these
// gateway settings were configuration, everything else a credential.
var legacyReadable = map[string]bool{
	"ANTHROPIC_BASE_URL": true,
	"ANTHROPIC_MODEL":    true,
	"OPENAI_BASE_URL":    true,
	"OPENAI_MODEL":       true,
	"GROK_BASE_URL":      true,
}

type VaultStore struct {
	mu      sync.RWMutex
	path    string
	aead    cipher.AEAD
	secrets map[string]*VaultSecret // owner + "\x00" + name
}

// LoadVault opens the vault at path. keyMaterial is hashed to the AES-256 key;
// rotating it makes existing ciphertexts undecryptable, which surfaces as
// "reconnect your provider".
func LoadVault(path, keyMaterial string) *VaultStore {
	sum := sha256.Sum256([]byte("pd-vault|" + keyMaterial))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		// Can't happen with a 32-byte key; guard anyway.
		log.Printf("vault: cipher init failed: %v (vault disabled)", err)
		return nil
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		log.Printf("vault: gcm init failed: %v (vault disabled)", err)
		return nil
	}
	vs := &VaultStore{path: path, aead: aead, secrets: map[string]*VaultSecret{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("vault: could not read %s: %v (starting empty)", path, err)
		}
		return vs
	}
	var loaded map[string]*VaultSecret
	if err := json.Unmarshal(raw, &loaded); err != nil {
		log.Printf("vault: corrupt store %s: %v (starting empty)", path, err)
		return vs
	}
	for k, s := range loaded {
		if s != nil && s.Cipher != "" {
			if s.Kind == "" {
				s.Kind = "secret"
				if legacyReadable[s.Name] {
					s.Kind = "env"
				}
			}
			vs.secrets[k] = s
		}
	}
	return vs
}

func vaultKey(owner, name string) string { return owner + "\x00" + name }

// Put stores or replaces a secret. The value is encrypted before it touches
// the struct so a persist can never leak plaintext.
func (vs *VaultStore) Put(owner, name, value string, secret, autoInject bool) (*VaultSecret, error) {
	if owner == "" || owner == "anon" {
		// Anonymous callers share one identity, so nothing stored would be theirs alone.
		return nil, fmt.Errorf("sign in to store secrets: open /dashboard/login and paste an API key (make dev prints one at start)")
	}
	if err := sandbox.ValidEnvName(name); err != nil {
		return nil, err
	}
	if value == "" {
		return nil, fmt.Errorf("secret value is empty")
	}
	nonce := make([]byte, vs.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := vs.aead.Seal(nil, nonce, []byte(value), []byte(owner+"|"+name))
	last4 := value
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	now := time.Now().Format(time.RFC3339)
	vs.mu.Lock()
	defer vs.mu.Unlock()
	kind := "env"
	if secret {
		kind = "secret"
	}
	s := &VaultSecret{
		Name: name, Owner: owner, Kind: kind, AutoInject: autoInject,
		Cipher: base64.StdEncoding.EncodeToString(append(nonce, sealed...)),
		Last4:  last4, CreatedAt: now, UpdatedAt: now,
	}
	if prev, ok := vs.secrets[vaultKey(owner, name)]; ok {
		s.CreatedAt = prev.CreatedAt
	}
	vs.secrets[vaultKey(owner, name)] = s
	return vs.publicOf(s), vs.save()
}

// Value decrypts a secret for injection.
func (vs *VaultStore) Value(owner, name string) (string, bool) {
	vs.mu.RLock()
	s, ok := vs.secrets[vaultKey(owner, name)]
	vs.mu.RUnlock()
	if !ok {
		return "", false
	}
	return vs.decrypt(s)
}

// decrypt takes no locks: List calls it while holding the read lock, and a
// re-entrant RLock can deadlock against a waiting writer.
func (vs *VaultStore) decrypt(s *VaultSecret) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(s.Cipher)
	if err != nil || len(raw) < vs.aead.NonceSize() {
		return "", false
	}
	plain, err := vs.aead.Open(nil, raw[:vs.aead.NonceSize()], raw[vs.aead.NonceSize():], []byte(s.Owner+"|"+s.Name))
	if err != nil {
		// Rotated PORTAL_VAULT_KEY; treat as absent so the UI says "reconnect",
		// never a 500.
		return "", false
	}
	return string(plain), true
}

// publicOf is public() plus the plaintext for kind "env" only.
func (vs *VaultStore) publicOf(s *VaultSecret) *VaultSecret {
	p := s.public()
	if s.Kind == "env" {
		if v, ok := vs.decrypt(s); ok {
			p.Value = v
		}
	}
	return p
}

// List returns owner's secrets with ciphertext stripped, name-sorted.
func (vs *VaultStore) List(owner string) []*VaultSecret {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	out := make([]*VaultSecret, 0)
	if owner == "" || owner == "anon" {
		return out
	}
	for _, s := range vs.secrets {
		if s.Owner == owner {
			out = append(out, vs.publicOf(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AutoInject returns the decrypted name→value of owner's auto_inject entries.
func (vs *VaultStore) AutoInject(owner string) map[string]string {
	out := map[string]string{}
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	for _, s := range vs.secrets {
		if s.Owner == owner && s.AutoInject {
			if v, ok := vs.decrypt(s); ok {
				out[s.Name] = v
			}
		}
	}
	return out
}

// SetAutoInject flips auto-injection without touching the value; false means
// owner has no such secret.
func (vs *VaultStore) SetAutoInject(owner, name string, autoInject bool) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	s, ok := vs.secrets[vaultKey(owner, name)]
	if !ok {
		return false
	}
	s.AutoInject = autoInject
	s.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := vs.save(); err != nil {
		log.Printf("vault: persisting auto_inject failed: %v", err)
	}
	return true
}

func (vs *VaultStore) Delete(owner, name string) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if _, ok := vs.secrets[vaultKey(owner, name)]; !ok {
		return false
	}
	delete(vs.secrets, vaultKey(owner, name))
	if err := vs.save(); err != nil {
		// The in-memory delete already happened; a failed persist means the
		// secret can reappear on restart.
		log.Printf("vault: persisting delete failed: %v", err)
	}
	return true
}

// public strips the fields clients must never see.
func (s *VaultSecret) public() *VaultSecret {
	return &VaultSecret{Name: s.Name, Kind: s.Kind, AutoInject: s.AutoInject, Last4: s.Last4, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

// save persists atomically via tmp+rename. Callers hold vs.mu.
func (vs *VaultStore) save() error {
	raw, err := json.Marshal(vs.secrets)
	if err != nil {
		return err
	}
	tmp := vs.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, vs.path)
}

// registerVaultRoutes mounts /v1/secrets, org-scoped by ownerID.
func registerVaultRoutes(r chi.Router, vault *VaultStore, ownerID func(*http.Request) string) {
	if vault == nil {
		return
	}
	r.Get("/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, vault.List(ownerID(r)))
	})
	r.Post("/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		// secret defaults to true so an old client that never sends it keeps sealing.
		var body struct {
			Name       string `json:"name"`
			Value      string `json:"value"`
			Secret     *bool  `json:"secret"`
			AutoInject *bool  `json:"auto_inject"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		// A rotation that does not mention auto_inject keeps it.
		autoInject := body.AutoInject != nil && *body.AutoInject
		if body.AutoInject == nil {
			for _, v := range vault.List(ownerID(r)) {
				if v.Name == body.Name {
					autoInject = v.AutoInject
				}
			}
		}
		s, err := vault.Put(ownerID(r), body.Name, body.Value, body.Secret == nil || *body.Secret, autoInject)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, s)
	})
	r.Patch("/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AutoInject *bool `json:"auto_inject"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AutoInject == nil {
			http.Error(w, `{"error":"auto_inject is required: true or false"}`, http.StatusBadRequest)
			return
		}
		name := chi.URLParam(r, "name")
		if !vault.SetAutoInject(ownerID(r), name, *body.AutoInject) {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"name": name, "auto_inject": *body.AutoInject})
	})
	r.Delete("/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !vault.Delete(ownerID(r), chi.URLParam(r, "name")) {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// resolveSecretRefs rewrites `secret_refs` into env entries from the caller's
// vault and strips the field before forwarding. An unresolvable ref fails the
// create loudly.
func resolveSecretRefs(raw []byte, vault *VaultStore, owner string) ([]byte, error) {
	if vault == nil || len(raw) == 0 {
		return raw, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, nil // not JSON; forward untouched
	}
	refsAny, _ := obj["secret_refs"].([]any)
	auto := vault.AutoInject(owner)
	if len(refsAny) == 0 && len(auto) == 0 {
		return raw, nil
	}
	// Explicit env and refs win over auto_inject on a name collision.
	env, _ := obj["env"].([]any)
	have := map[string]bool{}
	for _, e := range env {
		if kv, ok := e.(string); ok {
			name, _, _ := strings.Cut(kv, "=")
			have[name] = true
		}
	}
	for _, ra := range refsAny {
		name, _ := ra.(string)
		val, found := vault.Value(owner, name)
		if !found {
			return nil, fmt.Errorf("secret %q is not in your vault — connect it under Vault first", name)
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
