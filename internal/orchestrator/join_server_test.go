package orchestrator

import (
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestServerHasJoinStoreAndBootstrap(t *testing.T) {
	t.Setenv("NODE_BOOTSTRAP_DSN", "postgres://u:p@h:5432/db")
	t.Setenv("NODE_BOOTSTRAP_SECRET", "sekret")
	s := NewServer("apikey", "nodetoken")
	if s.join == nil {
		t.Fatal("server has no JoinStore")
	}
	tok := s.join.Mint(s.now(), cluster.JoinTokenTTL)
	if !s.join.Consume(tok, s.now()) {
		t.Fatal("server JoinStore consume failed")
	}
	if s.bootstrapDSN != "postgres://u:p@h:5432/db" || s.bootstrapSecret != "sekret" {
		t.Fatalf("bootstrap config not read: dsn=%q secret=%q", s.bootstrapDSN, s.bootstrapSecret)
	}
}

// The control plane seals with SANDBOXD_SECRET_KEY when NODE_BOOTSTRAP_SECRET
// is unset; its nodes must get the same key, or they seal nothing at all.
func TestJoinSecretDefaultsToTheControlPlaneKey(t *testing.T) {
	t.Setenv("NODE_BOOTSTRAP_SECRET", "")
	t.Setenv("SANDBOXD_SECRET_KEY", "cp-key")
	if s := NewServer("apikey", "nodetoken"); s.bootstrapSecret != "cp-key" {
		t.Fatalf("join secret = %q, want the control plane's SANDBOXD_SECRET_KEY", s.bootstrapSecret)
	}
	t.Setenv("NODE_BOOTSTRAP_SECRET", "node-key")
	if s := NewServer("apikey", "nodetoken"); s.bootstrapSecret != "node-key" {
		t.Fatalf("join secret = %q, want NODE_BOOTSTRAP_SECRET when it is set", s.bootstrapSecret)
	}
}
