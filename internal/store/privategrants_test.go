package store

import (
	"errors"
	"testing"
)

func TestPrivateGrantNamesAreUniquePerSandbox(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		org := "org-grants" + sfx
		a, b := "cg_a"+sfx, "cg_b"+sfx
		for _, c := range []Connector{{ID: a, Name: "a", OrgID: org}, {ID: b, Name: "b", OrgID: org}} {
			if err := st.InsertConnector(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		sb := "sbg" + sfx
		if err := st.AddPrivateGrant(ctx, PrivateGrant{SandboxID: sb, ConnectorID: a, Target: "db"}); err != nil {
			t.Fatal(err)
		}
		err := st.AddPrivateGrant(ctx, PrivateGrant{SandboxID: sb, ConnectorID: b, Target: "db"})
		if !errors.Is(err, ErrPrivateNameTaken) {
			t.Fatalf("second db on one sandbox: %v", err)
		}
		g, ok, err := st.PrivateGrantFor(ctx, sb, "db")
		if err != nil || !ok || g.ConnectorID != a || g.ConnectorName != "a" {
			t.Fatalf("grant for: %+v %v %v", g, ok, err)
		}
		if ok, _ := st.RemovePrivateGrant(ctx, sb, a, "db"); !ok {
			t.Fatal("remove")
		}
		if _, ok, _ := st.PrivateGrantFor(ctx, sb, "db"); ok {
			t.Fatal("grant survives remove")
		}
	})
}

func TestPrivateConnLogNewestFirst(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		cid := "cl" + sfx
		for i, id := range []string{"pc1" + sfx, "pc2" + sfx} {
			if err := st.InsertPrivateConn(ctx, PrivateConn{ID: id, OrgID: "o", SandboxID: "sb", ConnectorID: cid, Target: "db", Result: "ok", StartedAt: int64(i + 1), EndedAt: int64(i + 2), BytesIn: 5}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := st.PrivateConns(ctx, cid, 10)
		if err != nil || len(got) != 2 || got[0].ID != "pc2"+sfx {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func TestSandboxesGrantedConnectorAndSetPrivate(t *testing.T) {
	eachStore(t, func(t *testing.T, st *Store, sfx string) {
		ctx := t.Context()
		cid := "cs" + sfx
		if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "s", OrgID: "org-sgc" + sfx}); err != nil {
			t.Fatal(err)
		}
		for _, g := range []PrivateGrant{{SandboxID: "sbs1" + sfx, Target: "db"}, {SandboxID: "sbs1" + sfx, Target: "api"}, {SandboxID: "sbs2" + sfx, Target: "db"}} {
			g.ConnectorID = cid
			if err := st.AddPrivateGrant(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
		ids, err := st.SandboxesGrantedConnector(ctx, cid)
		if err != nil || len(ids) != 2 {
			t.Fatalf("granted sandboxes: %v %v", ids, err)
		}
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
