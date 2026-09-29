package store

import (
	"context"
	"encoding/json"
	"time"
)

// SandboxConfig is a sandbox's post-launch env, model and base URL, applied
// per exec so a change lands on the next command.
type SandboxConfig struct {
	Env     map[string]string `json:"env"`
	Model   string            `json:"model"`
	BaseURL string            `json:"base_url"`
}

// SandboxConfigFor returns a sandbox's config, empty when never set. env is
// encrypted at rest; open passes legacy plaintext rows through unchanged.
func (s *Store) SandboxConfigFor(ctx context.Context, sandboxID string) SandboxConfig {
	var envJSON, model, baseURL string
	err := s.queryRow(ctx,
		`SELECT env, model, base_url FROM sandbox_config WHERE sandbox_id = ?`, sandboxID,
	).Scan(&envJSON, &model, &baseURL)
	c := SandboxConfig{Env: map[string]string{}}
	if err != nil {
		return c
	}
	// A decrypt failure yields no env, never ciphertext injected as an API key.
	plain, err := s.crypt.open(envJSON)
	if err != nil {
		return c
	}
	_ = json.Unmarshal([]byte(plain), &c.Env)
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	c.Model, c.BaseURL = model, baseURL
	return c
}

// MergeSandboxConfig folds an update into the stored config. An empty env
// value removes that variable.
func (s *Store) MergeSandboxConfig(ctx context.Context, sandboxID, orgID string, env map[string]string, model, baseURL string) (SandboxConfig, error) {
	cur := s.SandboxConfigFor(ctx, sandboxID)
	for k, v := range env {
		if v == "" {
			delete(cur.Env, k)
			continue
		}
		cur.Env[k] = v
	}
	if model != "" {
		cur.Model = model
	}
	if baseURL != "" {
		cur.BaseURL = baseURL
	}
	blob, err := json.Marshal(cur.Env)
	if err != nil {
		return cur, err
	}
	enc, err := s.crypt.seal(string(blob))
	if err != nil {
		return cur, err
	}
	if _, err := s.exec(ctx, `DELETE FROM sandbox_config WHERE sandbox_id = ?`, sandboxID); err != nil {
		return cur, err
	}
	_, err = s.exec(ctx,
		`INSERT INTO sandbox_config (sandbox_id, org_id, env, model, base_url, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sandboxID, orgID, enc, cur.Model, cur.BaseURL, time.Now().Unix())
	return cur, err
}

// DeleteSandboxConfig drops a sandbox's configuration, credentials included.
func (s *Store) DeleteSandboxConfig(ctx context.Context, sandboxID string) error {
	_, err := s.exec(ctx, `DELETE FROM sandbox_config WHERE sandbox_id = ?`, sandboxID)
	return err
}
