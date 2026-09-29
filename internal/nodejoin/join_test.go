package nodejoin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchConfigAndRenderEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/join" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"node_id":"node-7","node_api_key":"sf_node_abc","orchestrator_url":"` +
			"http://orch" + `","postgres_dsn":"postgres://d","secret_key":"sek","zone":"par1","node_token":"toktok"}`))
	}))
	defer srv.Close()

	cfg, err := FetchConfig(t.Context(), srv.URL, "sfj_tok", "http://5.6.7.8:18080", "par1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if cfg.NodeID != "node-7" || cfg.NodeAPIKey != "sf_node_abc" {
		t.Fatalf("cfg = %+v", cfg)
	}

	env := RenderEnv(cfg)
	for _, want := range []string{
		"NODE_ID=node-7", "NODE_API_KEY=sf_node_abc", "ORCHESTRATOR_URL=http://orch",
		"SANDBOXD_DATABASE_URL=postgres://d", "SANDBOXD_SECRET_KEY=sek",
		"NODE_ZONE=par1", "NODE_URL=http://5.6.7.8:18080", "SANDBOXD_PRODUCTION=1",
		"ORCHESTRATOR_TOKEN=toktok",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q\n%s", want, env)
		}
	}
}

// A missing ORCHESTRATOR_TOKEN means a joined node 401s on every heartbeat.
func TestRenderEnvIsSufficientForHeartbeat(t *testing.T) {
	cfg := Config{
		NodeID:          "node-9",
		NodeAPIKey:      "sf_node_key",
		OrchestratorURL: "http://orch",
		PostgresDSN:     "postgres://d",
		SecretKey:       "sek",
		Zone:            "par1",
		NodeToken:       "toktok",
		advertiseURL:    "http://5.6.7.8:18080",
	}
	env := RenderEnv(cfg)
	for _, key := range []string{
		"ORCHESTRATOR_URL", "ORCHESTRATOR_TOKEN", "NODE_ID", "NODE_ZONE", "NODE_URL",
		"NODE_API_KEY", "SANDBOXD_DATABASE_URL", "SANDBOXD_SECRET_KEY", "SANDBOXD_PRODUCTION",
	} {
		if !strings.Contains(env, key+"=") {
			t.Fatalf("RenderEnv missing required var %q\n%s", key, env)
		}
	}
}

func TestFetchConfigErrorsOnRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid, expired, or already-used join token"}`))
	}))
	defer srv.Close()
	if _, err := FetchConfig(t.Context(), srv.URL, "sfj_bad", "http://x:18080", "par1"); err == nil {
		t.Fatal("expected an error on 401")
	}
}
