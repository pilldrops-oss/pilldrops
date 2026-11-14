package store

import (
	"errors"
	"testing"
)

func TestPrivateGrantNamesAreUniquePerSandbox(t *testing.T) {
	for name, st := range connectorStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			org := "org-grants-" + name
			for _, c := range []Connector{{ID: "cg_a" + name, Name: "a", OrgID: org}, {ID: "cg_b" + name, Name: "b", OrgID: org}} {
				if err := st.InsertConnector(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			sb := "sbg-" + name
			if err := st.AddPrivateGrant(ctx, PrivateGrant{SandboxID: sb, ConnectorID: "cg_a" + name, Target: "db"}); err != nil {
				t.Fatal(err)
			}
			err := st.AddPrivateGrant(ctx, PrivateGrant{SandboxID: sb, ConnectorID: "cg_b" + name, Target: "db"})
			if !errors.Is(err, ErrPrivateNameTaken) {
				t.Fatalf("second db on one sandbox: %v", err)
			}
			g, ok, err := st.PrivateGrantFor(ctx, sb, "db")
			if err != nil || !ok || g.ConnectorID != "cg_a"+name || g.ConnectorName != "a" {
				t.Fatalf("grant for: %+v %v %v", g, ok, err)
			}
			if ok, _ := st.RemovePrivateGrant(ctx, sb, "cg_a"+name, "db"); !ok {
				t.Fatal("remove")
			}
			if _, ok, _ := st.PrivateGrantFor(ctx, sb, "db"); ok {
				t.Fatal("grant survives remove")
			}
		})
	}
}

func TestPrivateConnLogNewestFirst(t *testing.T) {
	for name, st := range connectorStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			cid := "cl_" + name
			for i, id := range []string{"pc1" + name, "pc2" + name} {
				if err := st.InsertPrivateConn(ctx, PrivateConn{ID: id, OrgID: "o", SandboxID: "sb", ConnectorID: cid, Target: "db", Result: "ok", StartedAt: int64(i + 1), EndedAt: int64(i + 2), BytesIn: 5}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := st.PrivateConns(ctx, cid, 10)
			if err != nil || len(got) != 2 || got[0].ID != "pc2"+name {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestSandboxesGrantedConnectorAndSetPrivate(t *testing.T) {
	for name, st := range connectorStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			cid := "cs_" + name
			if err := st.InsertConnector(ctx, Connector{ID: cid, Name: "s", OrgID: "org-sgc-" + name}); err != nil {
				t.Fatal(err)
			}
			for _, g := range []PrivateGrant{{SandboxID: "sbs1" + name, Target: "db"}, {SandboxID: "sbs1" + name, Target: "api"}, {SandboxID: "sbs2" + name, Target: "db"}} {
				g.ConnectorID = cid
				if err := st.AddPrivateGrant(ctx, g); err != nil {
					t.Fatal(err)
				}
			}
			ids, err := st.SandboxesGrantedConnector(ctx, cid)
			if err != nil || len(ids) != 2 {
				t.Fatalf("granted sandboxes: %v %v", ids, err)
			}
			if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sbp" + name, Template: "t", OrgID: "o", State: "running"}); err != nil {
				t.Fatal(err)
			}
			const targets = `[{"name":"db","port":5432}]`
			if err := st.SetSandboxPrivate(ctx, "sbp"+name, targets); err != nil {
				t.Fatal(err)
			}
			rec, ok, err := st.SandboxByID(ctx, "sbp"+name)
			if err != nil || !ok || rec.Private != targets {
				t.Fatalf("private = %q %v %v", rec.Private, ok, err)
			}
		})
	}
}
