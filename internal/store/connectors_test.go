package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// eachStore runs fn on sqlite and, when SANDBOXD_TEST_POSTGRES_URL is set, on
// Postgres. sfx is unique per run and ends every id and org the test inserts,
// so the Postgres cleanup can delete exactly those rows.
func eachStore(t *testing.T, fn func(t *testing.T, st *Store, sfx string)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		st, err := Open(t.Context(), filepath.Join(t.TempDir(), "conn.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		fn(t, st, "sqlite")
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL")
		if dsn == "" {
			t.Skip("set SANDBOXD_TEST_POSTGRES_URL to run Postgres tests")
		}
		st, err := Open(t.Context(), dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		sfx := fmt.Sprintf("-%d", time.Now().UnixNano())
		t.Cleanup(func() {
			for table, col := range map[string]string{
				"connectors": "id", "connector_keys": "connector_id", "sandbox_private_grants": "sandbox_id", "private_grants": "subject",
				"private_connections": "connector_id", "sandboxes": "id",
			} {
				_, _ = st.db.Exec(st.rebind(`DELETE FROM `+table+` WHERE `+col+` LIKE ?`), "%"+sfx)
			}
		})
		fn(t, st, sfx)
	})
}

func TestConnectorLifecycle(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org := "org-lifecycle" + sfx
		c := Connector{ID: "cn_1" + sfx, Name: "prod", OrgID: org, CreatedAt: 10}
		if err := st.InsertConnector(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertConnector(ctx, Connector{ID: "cn_2" + sfx, Name: "prod", OrgID: org, CreatedAt: 11}); !errors.Is(err, ErrConnectorNameTaken) {
			t.Fatalf("duplicate name: %v", err)
		}
		ts := []ConnectorTarget{{Name: "db", Port: 5432}, {Name: "api", Port: 443, Host: "internal.corp"}}
		if err := st.SetConnectorAnnounced(ctx, c.ID, ts, nil, nil, 20); err != nil {
			t.Fatal(err)
		}
		got, ok, err := st.ConnectorByName(ctx, org, "prod")
		if err != nil || !ok || len(got.Targets) != 2 || got.Targets[1].Host != "internal.corp" || got.LastSeenAt != 20 {
			t.Fatalf("by name: %+v %v %v", got, ok, err)
		}
		if _, ok, _ := st.ConnectorByID(ctx, "other-org", c.ID); ok {
			t.Fatal("another org read the connector")
		}
		_, plain, err := st.CreateConnectorKey(ctx, c.ID, 30)
		if err != nil || !strings.HasPrefix(plain, "pd_conn_") || len(plain) != len("pd_conn_")+48 {
			t.Fatalf("key: %q %v", plain, err)
		}
		got, keyID, ok, _ := st.ConnectorForKey(ctx, plain, 31)
		if !ok || got.ID != c.ID || keyID == "" {
			t.Fatal("key does not resolve")
		}
		if live, err := st.ConnectorKeyLive(ctx, c.ID, keyID); !live || err != nil {
			t.Fatalf("key not live: %v %v", live, err)
		}
		if _, _, ok, _ := st.ConnectorForKey(ctx, plain+"0", 31); ok {
			t.Fatal("wrong key resolved")
		}
		if err := st.AddPrivateGrant(ctx, PrivateGrant{SandboxID: "sb1" + sfx, ConnectorID: c.ID, Target: "db", CreatedAt: 40}); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.RevokeConnector(ctx, org, c.ID, 50); !ok || err != nil {
			t.Fatalf("revoke: %v %v", ok, err)
		}
		if _, _, ok, _ := st.ConnectorForKey(ctx, plain, 51); ok {
			t.Fatal("key survives connector revoke")
		}
		if gs, _ := st.PrivateGrants(ctx, "sb1"+sfx); len(gs) != 0 {
			t.Fatalf("grants survive revoke: %+v", gs)
		}
		if n, _ := st.CountConnectors(ctx, org); n != 0 {
			t.Fatalf("count after revoke = %d", n)
		}
		if err := st.InsertConnector(ctx, Connector{ID: "cn_3" + sfx, Name: "prod", OrgID: org, CreatedAt: 60}); err != nil {
			t.Fatalf("name not freed by revoke: %v", err)
		}
	})
}
