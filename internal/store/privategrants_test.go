package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func addGrants(t *testing.T, st *Store, gs ...Grant) []Grant {
	t.Helper()
	out := make([]Grant, 0, len(gs))
	for _, g := range gs {
		got, err := st.AddGrant(t.Context(), g)
		if err != nil {
			t.Fatalf("add %+v: %v", g, err)
		}
		out = append(out, got)
	}
	return out
}

func TestEffectiveGrantsJoinSandboxAndTags(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org, cid := "org-eff"+sfx, "ce"+sfx
		if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "prod", OrgID: org}); err != nil {
			t.Fatal(err)
		}
		sb, tag := "sbe"+sfx, "backend"+sfx
		addGrants(t, st,
			Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectSandbox, Subject: sb, Kind: GrantKindTarget, Value: "db"},
			Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectTag, Subject: tag, Kind: GrantKindSubnet, Value: "10.0.5.0/24"},
			Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectTag, Subject: "other" + sfx, Kind: GrantKindDNS, Value: "corp.internal"},
		)
		dup := Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectSandbox, Subject: sb, Kind: GrantKindTarget, Value: "db"}
		if _, err := st.AddGrant(ctx, dup); !errors.Is(err, ErrGrantExists) {
			t.Fatalf("duplicate grant: %v", err)
		}
		gs, err := st.EffectiveGrants(ctx, org, sb, []string{tag})
		if err != nil || len(gs) != 2 {
			t.Fatalf("effective: %+v %v", gs, err)
		}
		if gs[0].Kind != GrantKindSubnet || gs[0].ConnectorName != "prod" || gs[0].SubjectKind != GrantSubjectTag || gs[1].Value != "db" {
			t.Fatalf("order or join: %+v", gs)
		}
		if gs, _ := st.EffectiveGrants(ctx, "org-other"+sfx, sb, []string{tag}); len(gs) != 0 {
			t.Fatalf("another org's grants: %+v", gs)
		}
		if gs, _ := st.EffectiveGrants(ctx, org, sb, nil); len(gs) != 1 {
			t.Fatalf("no tags: %+v", gs)
		}
		if gs, _ := st.TagGrants(ctx, org, tag); len(gs) != 1 || gs[0].Value != "10.0.5.0/24" {
			t.Fatalf("tag grants: %+v", gs)
		}
		if n, _ := st.CountTagGrants(ctx, org); n != 2 {
			t.Fatalf("tag grant count %d", n)
		}
		if gs, _ := st.GrantsOnConnector(ctx, cid); len(gs) != 3 {
			t.Fatalf("grants on connector: %+v", gs)
		}
	})
}

func TestSandboxTargetGrantsMirrorTheOldTable(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org, cid, sb := "org-mir"+sfx, "cm"+sfx, "sbm"+sfx
		if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "m", OrgID: org}); err != nil {
			t.Fatal(err)
		}
		old := func() int {
			t.Helper()
			var n int
			if err := st.queryRow(ctx, `SELECT COUNT(*) FROM sandbox_private_grants WHERE sandbox_id = ?`, sb).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		gs := addGrants(t, st,
			Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectSandbox, Subject: sb, Kind: GrantKindTarget, Value: "db"},
			Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectSandbox, Subject: sb, Kind: GrantKindSubnet, Value: "10.0.0.0/16"},
		)
		if n := old(); n != 1 {
			t.Fatalf("old table holds %d rows, want the target only", n)
		}
		found, ok, err := st.FindGrant(ctx, GrantSubjectSandbox, sb, cid, GrantKindTarget, "db")
		if err != nil || !ok || found.ID != gs[0].ID || !strings.HasPrefix(found.ID, "pg_") {
			t.Fatalf("find: %+v %v %v", found, ok, err)
		}
		if err := st.RemoveGrant(ctx, gs[0]); err != nil {
			t.Fatal(err)
		}
		if n := old(); n != 0 {
			t.Fatalf("revoke left %d old rows", n)
		}
		if _, ok, _ := st.GrantByID(ctx, org, gs[0].ID); ok {
			t.Fatal("grant survives remove")
		}
		if _, ok, _ := st.GrantByID(ctx, "org-other"+sfx, gs[1].ID); ok {
			t.Fatal("another org read the grant by id")
		}
	})
}

func TestPhaseOneGrantsCopyIntoPrivateGrants(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org, cid, sb := "org-cp"+sfx, "cc"+sfx, "sbc"+sfx
		if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "c", OrgID: org}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.exec(ctx, `INSERT INTO sandbox_private_grants (sandbox_id, connector_id, target, created_at) VALUES (?, ?, ?, ?)`, sb, cid, "db", 7); err != nil {
			t.Fatal(err)
		}
		stmt := migrationCopyStatement(t, st)
		for range 2 { // re-running must not duplicate
			if _, err := st.db.ExecContext(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
		gs, err := st.SandboxGrants(ctx, sb)
		if err != nil || len(gs) != 1 || gs[0].Value != "db" || gs[0].OrgID != org || gs[0].CreatedAt != 7 || !strings.HasPrefix(gs[0].ID, "pg_") {
			t.Fatalf("copied: %+v %v", gs, err)
		}
	})
}

// migrationCopyStatement is migration 93's statement copying phase 1 grants.
func migrationCopyStatement(t *testing.T, st *Store) string {
	t.Helper()
	for _, m := range migrations {
		if m.name != "private_routes" {
			continue
		}
		stmts := m.sqlite
		if st.dialect == dialectPostgres {
			stmts = m.postgres
		}
		for _, s := range stmts {
			if strings.HasPrefix(strings.TrimSpace(s), "INSERT INTO private_grants") {
				return s
			}
		}
	}
	t.Fatal("migration private_routes has no copy statement")
	return ""
}

func TestPrivateConnLogNewestFirst(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		cid := "cl" + sfx
		for i, id := range []string{"pc1" + sfx, "pc2" + sfx} {
			if err := st.InsertPrivateConn(ctx, PrivateConn{ID: id, OrgID: "o", SandboxID: "sb", ConnectorID: cid, Target: "10.0.5.0/24", Dest: "10.0.5.7:5432", Result: "ok", StartedAt: int64(i + 1), EndedAt: int64(i + 2), BytesIn: 5}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := st.PrivateConns(ctx, cid, 10)
		if err != nil || len(got) != 2 || got[0].ID != "pc2"+sfx || got[0].Dest != "10.0.5.7:5432" {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func TestSandboxPrivateColumnsAndTagMembers(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org := "org-tm" + sfx
		for id, nets := range map[string]string{"sbt1" + sfx: `["backend","web"]`, "sbt2" + sfx: `["web"]`, "sbt3" + sfx: `[]`} {
			if err := st.SaveSandbox(ctx, SandboxRecord{ID: id, Template: "t", OrgID: org, State: "running", Networks: nets}); err != nil {
				t.Fatal(err)
			}
		}
		ids, err := st.SandboxesInTag(ctx, org, "backend")
		if err != nil || len(ids) != 1 || ids[0] != "sbt1"+sfx {
			t.Fatalf("members: %v %v", ids, err)
		}
		if ids, _ := st.SandboxesInTag(ctx, "org-x"+sfx, "web"); len(ids) != 0 {
			t.Fatalf("another org's members: %v", ids)
		}
		const routes = `{"subnets":["10.0.5.0/24"],"dns":["corp.internal"]}`
		if err := st.SetSandboxPrivateRoutes(ctx, "sbt1"+sfx, routes); err != nil {
			t.Fatal(err)
		}
		// The node's own save must not undo what the control plane wrote.
		if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sbt1" + sfx, Template: "t", OrgID: org, State: "paused", Networks: `["backend"]`}); err != nil {
			t.Fatal(err)
		}
		rec, ok, err := st.SandboxByID(ctx, "sbt1"+sfx)
		if err != nil || !ok || rec.PrivateRoutes != routes {
			t.Fatalf("private_routes = %q %v %v", rec.PrivateRoutes, ok, err)
		}
		if err := st.RecordPlacedSandbox(ctx, SandboxRecord{ID: "sbt4" + sfx, Template: "t", OrgID: org, State: "running", Networks: `["backend"]`}); err != nil {
			t.Fatal(err)
		}
		if ids, _ := st.SandboxesInTag(ctx, org, "backend"); len(ids) != 2 {
			t.Fatalf("a placed sandbox's networks were not recorded: %v", ids)
		}
		if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sbt5" + sfx, Template: "t", OrgID: org, State: "destroyed", Networks: `["backend"]`}); err != nil {
			t.Fatal(err)
		}
		if ids, _ := st.SandboxesInTag(ctx, org, "backend"); len(ids) != 2 {
			t.Fatalf("an ended sandbox was listed: %v", ids)
		}
	})
}

func TestConnectorAnnouncementRoundTrips(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		cid := "ca" + sfx
		if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "a", OrgID: "org-ann" + sfx}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetConnectorAnnounced(ctx, cid, []ConnectorTarget{{Name: "db", Port: 5432}}, []string{"10.0.0.0/16"}, []string{"corp.internal"}, 9); err != nil {
			t.Fatal(err)
		}
		c, ok, err := st.ConnectorByID(ctx, "", cid)
		if err != nil || !ok || len(c.Targets) != 1 || len(c.Subnets) != 1 || c.Subnets[0] != "10.0.0.0/16" || len(c.Suffixes) != 1 || c.LastSeenAt != 9 {
			t.Fatalf("%+v %v %v", c, ok, err)
		}
	})
}

// Every destroy path ends in DeleteSandbox; a grant that outlived its sandbox
// would route a later sandbox reusing the id.
func TestDeleteSandboxDropsItsGrants(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org, cid := "org-del"+sfx, "cd"+sfx
		if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "d", OrgID: org}); err != nil {
			t.Fatal(err)
		}
		for _, del := range []func(context.Context, string) error{st.DeleteSandbox, st.ForgetSandbox} {
			gone, kept := "sbd1"+sfx, "sbd2"+sfx
			for _, id := range []string{gone, kept} {
				if err := st.SaveSandbox(ctx, SandboxRecord{ID: id, Template: "t", OrgID: org, State: "running"}); err != nil {
					t.Fatal(err)
				}
				addGrants(t, st,
					Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectSandbox, Subject: id, Kind: GrantKindTarget, Value: "db"},
					Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectSandbox, Subject: id, Kind: GrantKindDNS, Value: "corp.internal"},
				)
			}
			if err := del(ctx, gone); err != nil {
				t.Fatal(err)
			}
			if gs, err := st.SandboxGrants(ctx, gone); err != nil || len(gs) != 0 {
				t.Fatalf("grants outlived the sandbox: %v %v", gs, err)
			}
			var old int
			if err := st.queryRow(ctx, `SELECT COUNT(*) FROM sandbox_private_grants WHERE sandbox_id = ?`, gone).Scan(&old); err != nil {
				t.Fatal(err)
			}
			if old != 0 {
				t.Fatalf("old table kept %d rows", old)
			}
			if gs, err := st.SandboxGrants(ctx, kept); err != nil || len(gs) != 2 {
				t.Fatalf("another sandbox's grants: %v %v", gs, err)
			}
			if err := st.DeleteSandbox(ctx, kept); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestRevokeConnectorDropsGrantsInBothTables(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org, cid := "org-rv"+sfx, "cr"+sfx
		if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "r", OrgID: org}); err != nil {
			t.Fatal(err)
		}
		addGrants(t, st,
			Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectSandbox, Subject: "sbr" + sfx, Kind: GrantKindTarget, Value: "db"},
			Grant{OrgID: org, ConnectorID: cid, SubjectKind: GrantSubjectTag, Subject: "t" + sfx, Kind: GrantKindSubnet, Value: "10.0.0.0/16"},
		)
		if ok, err := st.RevokeConnector(ctx, org, cid, 5); err != nil || !ok {
			t.Fatalf("revoke: %v %v", ok, err)
		}
		var n int
		if err := st.queryRow(ctx, `SELECT COUNT(*) FROM private_grants WHERE connector_id = ?`, cid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		var old int
		if err := st.queryRow(ctx, `SELECT COUNT(*) FROM sandbox_private_grants WHERE connector_id = ?`, cid).Scan(&old); err != nil {
			t.Fatal(err)
		}
		if n != 0 || old != 0 {
			t.Fatalf("left %d new and %d old grant rows", n, old)
		}
	})
}

func TestSetSandboxPrivateRoundTrips(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		id := "sbp" + sfx
		if err := st.SaveSandbox(ctx, SandboxRecord{ID: id, Template: "t", OrgID: "o", State: "running"}); err != nil {
			t.Fatal(err)
		}
		const targets = `[{"name":"db","port":5432}]`
		if err := st.SetSandboxPrivate(ctx, id, targets); err != nil {
			t.Fatal(err)
		}
		rec, ok, err := st.SandboxByID(ctx, id)
		if err != nil || !ok || rec.Private != targets {
			t.Fatalf("private = %q %v %v", rec.Private, ok, err)
		}
	})
}
