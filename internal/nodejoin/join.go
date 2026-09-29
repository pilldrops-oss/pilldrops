// Package nodejoin redeems a join token at the orchestrator and renders the
// resulting bootstrap config into a sandboxd env file.
package nodejoin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Config is the bootstrap payload returned by POST /v1/join.
type Config struct {
	NodeID          string `json:"node_id"`
	NodeAPIKey      string `json:"node_api_key"`
	OrchestratorURL string `json:"orchestrator_url"`
	PostgresDSN     string `json:"postgres_dsn"`
	SecretKey       string `json:"secret_key"`
	Zone            string `json:"zone"`
	NodeToken       string `json:"node_token"` // presented on every heartbeat
	// OrgAssertPublicKey is the control plane's Ed25519 verifying key; a node
	// can check an X-Org-Override assertion but cannot mint one.
	OrgAssertPublicKey string `json:"org_assert_public_key"`
	advertiseURL       string // filled by FetchConfig for RenderEnv
}

// FetchConfig redeems the token at orchestratorURL and returns the node config.
func FetchConfig(ctx context.Context, orchestratorURL, token, advertiseURL, zone string) (Config, error) {
	reqBody, err := json.Marshal(map[string]string{
		"token": token, "advertise_url": advertiseURL, "zone": zone,
	})
	if err != nil {
		return Config{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", orchestratorURL+"/v1/join", bytes.NewReader(reqBody))
	if err != nil {
		return Config{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return Config{}, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Config{}, fmt.Errorf("reading join response: %w", err)
	}
	if resp.StatusCode != 200 {
		return Config{}, fmt.Errorf("join rejected (HTTP %d): %s", resp.StatusCode, string(raw))
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	cfg.advertiseURL = advertiseURL
	return cfg, nil
}

// RenderEnv produces the /etc/sandboxd.env contents. SANDBOXD_RUNTIME is left
// unset so sandboxd auto-selects Firecracker or Docker.
func RenderEnv(c Config) string {
	pairs := [][2]string{
		{"NODE_ID", c.NodeID},
		{"NODE_ZONE", c.Zone},
		{"NODE_URL", c.advertiseURL},
		{"NODE_API_KEY", c.NodeAPIKey},
		{"ORCHESTRATOR_URL", c.OrchestratorURL},
		{"ORCHESTRATOR_TOKEN", c.NodeToken},
		{"SANDBOXD_DATABASE_URL", c.PostgresDSN},
		{"SANDBOXD_SECRET_KEY", c.SecretKey},
		{"ORG_ASSERT_PUBLIC_KEY", c.OrgAssertPublicKey},
		{"SANDBOXD_PRODUCTION", "1"},
	}
	var b bytes.Buffer
	for _, p := range pairs {
		fmt.Fprintf(&b, "%s=%s\n", p[0], p[1])
	}
	return b.String()
}
