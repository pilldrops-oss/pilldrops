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
