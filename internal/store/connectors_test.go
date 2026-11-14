package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func connectorStores(t *testing.T) map[string]*Store {
	t.Helper()
	out := map[string]*Store{}
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "conn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	out["sqlite"] = st
	if dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL"); dsn != "" {
		pg, err := Open(t.Context(), dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() { _ = pg.Close() })
		out["postgres"] = pg
	}
	return out
}

func TestConnectorLifecycle(t *testing.T) {
	for name, st := range connectorStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			org := "org-" + strings.ReplaceAll(t.Name(), "/", "-")
			c := Connector{ID: "cn_" + name + "1", Name: "prod", OrgID: org, CreatedAt: 10}
			if err := st.InsertConnector(ctx, c); err != nil {
				t.Fatal(err)
			}
			if err := st.InsertConnector(ctx, Connector{ID: "cn_" + name + "2", Name: "prod", OrgID: org, CreatedAt: 11}); !errors.Is(err, ErrConnectorNameTaken) {
				t.Fatalf("duplicate name: %v", err)
			}
			ts := []ConnectorTarget{{Name: "db", Port: 5432}, {Name: "api", Port: 443, Host: "internal.corp"}}
			if err := st.SetConnectorTargets(ctx, c.ID, ts, 20); err != nil {
				t.Fatal(err)
			}
			got, ok, err := st.ConnectorByName(ctx, org, "prod")
			if err != nil || !ok || len(got.Targets) != 2 || got.Targets[1].Host != "internal.corp" || got.LastSeenAt != 20 {
				t.Fatalf("by name: %+v %v %v", got, ok, err)
			}
			if _, ok, _ := st.ConnectorByID(ctx, "other-org", c.ID); ok {
				t.Fatal("another org read the connector")
			}
			key, plain, err := st.CreateConnectorKey(ctx, c.ID, 30)
			if err != nil || !strings.HasPrefix(plain, "pd_conn_") || len(plain) != len("pd_conn_")+48 {
				t.Fatalf("key: %q %v", plain, err)
			}
			if got, ok, _ := st.ConnectorForKey(ctx, plain, 31); !ok || got.ID != c.ID {
				t.Fatal("key does not resolve")
			}
			if _, ok, _ := st.ConnectorForKey(ctx, plain+"0", 31); ok {
				t.Fatal("wrong key resolved")
			}
			if err := st.AddPrivateGrant(ctx, PrivateGrant{SandboxID: "sb1", ConnectorID: c.ID, Target: "db", CreatedAt: 40}); err != nil {
				t.Fatal(err)
			}
			if ok, err := st.RevokeConnector(ctx, org, c.ID, 50); !ok || err != nil {
				t.Fatalf("revoke: %v %v", ok, err)
			}
			if _, ok, _ := st.ConnectorForKey(ctx, plain, 51); ok {
				t.Fatal("key survives connector revoke")
			}
			if gs, _ := st.PrivateGrants(ctx, "sb1"); len(gs) != 0 {
				t.Fatalf("grants survive revoke: %+v", gs)
			}
			if n, _ := st.CountConnectors(ctx, org); n != 0 {
				t.Fatalf("count after revoke = %d", n)
			}
			if err := st.InsertConnector(ctx, Connector{ID: "cn_" + name + "3", Name: "prod", OrgID: org, CreatedAt: 60}); err != nil {
				t.Fatalf("name not freed by revoke: %v", err)
			}
			_ = key
		})
	}
}
