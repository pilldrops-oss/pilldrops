package store

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

// Postgres only, on a scratch database: SQLite has no schema ACLs, and the role
// and revoke would outlive the test on the shared database.
func TestANodeRoleWithoutSchemaCreateCanOpenAMigratedStore(t *testing.T) {
	dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set SANDBOXD_TEST_POSTGRES_URL to run Postgres tests")
	}
	admin, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	suffix := time.Now().UnixNano()
	dbName := fmt.Sprintf("noderole_%d", suffix)
	roleName := fmt.Sprintf("noderole_user_%d", suffix)
	if _, err := admin.db.Exec(`CREATE DATABASE ` + dbName); err != nil {
		admin.Close()
		t.Skipf("cannot create a scratch database (%v) — needs a superuser DSN", err)
	}
	defer func() {
		if _, err := admin.db.Exec(`DROP DATABASE IF EXISTS ` + dbName + ` WITH (FORCE)`); err != nil {
			t.Logf("cleanup: drop database %s: %v", dbName, err)
		}
		if _, err := admin.db.Exec(`DROP ROLE IF EXISTS ` + roleName); err != nil {
			t.Logf("cleanup: drop role %s: %v", roleName, err)
		}
		admin.Close()
	}()
	if _, err := admin.db.Exec(fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'noderolepw'`, roleName)); err != nil {
		t.Skipf("cannot create a scratch role (%v) — needs a superuser DSN", err)
	}

	// Migrate with the privileged DSN first, so the node sees a schema at head.
	scratchDSN := swapDBName(dsn, dbName)
	cp, err := Open(t.Context(), scratchDSN)
	if err != nil {
		t.Fatalf("control-plane open of the scratch database: %v", err)
	}
	cp.Close()

	// The node role's grants: ordinary tables, SELECT on the artifact index, no DDL.
	for _, stmt := range []string{
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, dbName, roleName),
	} {
		if _, err := admin.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	grantConn, err := Open(t.Context(), scratchDSN)
	if err != nil {
		t.Fatalf("reopen scratch as admin: %v", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, roleName),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`, roleName),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, roleName),
		fmt.Sprintf(`REVOKE INSERT, UPDATE, DELETE ON template_builds, artifacts FROM %s`, roleName),
		fmt.Sprintf(`REVOKE CREATE ON SCHEMA public FROM %s`, roleName),
		`REVOKE CREATE ON SCHEMA public FROM PUBLIC`,
	} {
		if _, err := grantConn.db.Exec(stmt); err != nil {
			grantConn.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// Prove the revoke took, or a green result means nothing.
	var canCreate bool
	if err := grantConn.db.QueryRow(
		`SELECT has_schema_privilege($1, 'public', 'CREATE')`, roleName).Scan(&canCreate); err != nil {
		grantConn.Close()
		t.Fatalf("introspect schema privilege: %v", err)
	}
	grantConn.Close()
	if canCreate {
		t.Fatalf("the scratch role still holds CREATE on schema public, so this test could not fail")
	}

	nodeDSN, err := withUser(scratchDSN, roleName, "noderolepw")
	if err != nil {
		t.Fatalf("build node DSN: %v", err)
	}
	node, err := Open(t.Context(), nodeDSN)
	if err != nil {
		t.Fatalf("a node with the least-privilege role could not open an already-migrated store: %v\n"+
			"deploy/postgres-node-role.sql hands every fleet node exactly this role, so this is "+
			"every clustered node failing to boot — not a test-only configuration", err)
	}
	defer node.Close()

	// The create path reads the artifact index with this role.
	if _, err := node.ReadyBuildForTemplate(t.Context(), "nothing-published"); err != nil && !errors.Is(err, ErrNoReadyBuild) {
		t.Fatalf("the node role could not query template_builds: %v", err)
	}
}

// withUser rewrites the credentials of a postgres DSN.
func withUser(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
