package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// newPrivateDaemon backs the daemon with a real store, since private targets
// live in a row the control plane writes too.
func newPrivateDaemon(t *testing.T, privateAccess bool) (*Daemon, *runtimetest.Fake, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake := runtimetest.NewFake()
	fake.PrivateAccess = privateAccess
	d := New(Config{
		Runtime: fake, Store: StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Minute, HardTTL: time.Hour,
	})
	return d, fake, st
}

func storedPrivate(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	rec, ok, err := st.SandboxByID(t.Context(), id)
	if err != nil || !ok {
		t.Fatalf("row %s: %v %v", id, ok, err)
	}
	return rec.Private
}

var dbTarget = []sandbox.PrivateTarget{{Name: "db", Port: 5432}}

func TestSetPrivatePersistsAndPushes(t *testing.T) {
	d, fake, st := newPrivateDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.SetPrivate(t.Context(), id, dbTarget); err != nil {
		t.Fatal(err)
	}
	if got := fake.Private[id]; len(got) != 1 || got[0].Name != "db" {
		t.Fatalf("runtime got %+v", got)
	}
	if p := storedPrivate(t, st, id); !strings.Contains(p, `"db"`) {
		t.Fatalf("not persisted: %q", p)
	}
	if err := d.SetPrivate(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Private[id]; ok || storedPrivate(t, st, id) != "[]" {
		t.Fatalf("clear left %+v / %q", fake.Private[id], storedPrivate(t, st, id))
	}
}

func TestSetPrivateRefusedWithoutCapability(t *testing.T) {
	d, _, _ := newPrivateDaemon(t, false)
	id := createRunning(t, d).ID
	if err := d.SetPrivate(t.Context(), id, dbTarget); !errors.Is(err, ErrPrivateUnsupported) {
		t.Fatalf("got %v", err)
	}
	if err := d.SetPrivate(t.Context(), id, nil); err != nil {
		t.Fatalf("clearing on a node without private access: %v", err)
	}
}

func TestForkDoesNotCopyPrivate(t *testing.T) {
	d, fake, st := newPrivateDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.SetPrivate(t.Context(), id, dbTarget); err != nil {
		t.Fatal(err)
	}
	res, err := d.Fork(t.Context(), id, ForkSpec{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	child := res.Child.ID
	if len(fake.Private[child]) != 0 || storedPrivate(t, st, child) != "[]" {
		t.Fatal("fork inherited private targets")
	}
}

func TestResumeRepushesPrivate(t *testing.T) {
	d, fake, st := newPrivateDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.SetPrivate(t.Context(), id, dbTarget); err != nil {
		t.Fatal(err)
	}
	if err := d.Pause(t.Context(), id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	fake.Private = nil
	if err := d.Resume(t.Context(), id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := fake.Private[id]; len(got) != 1 {
		t.Fatalf("after resume: %+v", got)
	}
	if !strings.Contains(storedPrivate(t, st, id), `"db"`) {
		t.Fatal("resume's save dropped the stored targets")
	}
}

// The control plane writes the row while the node holds the paused sandbox;
// resume must apply that row, and the node's own saves must not undo it.
func TestGrantWhilePausedAppliesOnResume(t *testing.T) {
	d, fake, st := newPrivateDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.Pause(t.Context(), id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := st.SetSandboxPrivate(t.Context(), id, `[{"name":"db","port":5432}]`); err != nil {
		t.Fatal(err)
	}
	if err := d.Resume(t.Context(), id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := fake.Private[id]; len(got) != 1 || got[0].Name != "db" {
		t.Fatalf("after resume: %+v", got)
	}
	if !strings.Contains(storedPrivate(t, st, id), `"db"`) {
		t.Fatal("the node's save clobbered the control plane's private targets")
	}
}

func TestCheckpointRestoreRepushesPrivate(t *testing.T) {
	d, fake, _ := newPrivateDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.SetPrivate(t.Context(), id, dbTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Checkpoint(t.Context(), id, "c1"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	fake.Private = nil
	if _, err := d.RestoreCheckpoint(t.Context(), id, "c1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fake.Private[id]; len(got) != 1 {
		t.Fatalf("after checkpoint restore: %+v", got)
	}
}

// A move that lands a granted sandbox on a node without private access must
// not undo itself with Destroy: the id is the original's, still running on the
// source, and Destroy would delete its row and backups.
func TestMovedInCreateKeepsTheRowWhenPrivateCannotApply(t *testing.T) {
	d, _, st := newPrivateDaemon(t, false)
	const id = "sb-1790000000000-0123abcd"
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{ID: id, OrgID: "o", Template: "base", State: "running", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSandboxPrivate(t.Context(), id, `[{"name":"db","port":5432}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(t.Context(), CreateSpec{Template: "base", OrgID: "o", ID: id}); err != nil {
		t.Fatalf("moved-in create: %v", err)
	}
	if !strings.Contains(storedPrivate(t, st, id), `"db"`) {
		t.Fatal("the moved sandbox's stored targets were lost")
	}
}

var corpRoutes = sandbox.PrivateRoutes{Subnets: []string{"10.0.5.0/24"}, DNS: []string{"corp.internal"}}

func newRoutesDaemon(t *testing.T, routes bool) (*Daemon, *runtimetest.Fake, *store.Store) {
	t.Helper()
	d, fake, st := newPrivateDaemon(t, true)
	fake.PrivateRoutes = routes
	return d, fake, st
}

func storedRoutes(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	rec, ok, err := st.SandboxByID(t.Context(), id)
	if err != nil || !ok {
		t.Fatalf("row %s: %v %v", id, ok, err)
	}
	return rec.PrivateRoutes
}

func TestSetPrivateRoutesPersistsAndPushes(t *testing.T) {
	d, fake, st := newRoutesDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.SetPrivateRoutes(t.Context(), id, corpRoutes); err != nil {
		t.Fatal(err)
	}
	if got := fake.Routes[id]; len(got.Subnets) != 1 || got.DNS[0] != "corp.internal" {
		t.Fatalf("runtime got %+v", got)
	}
	if p := storedRoutes(t, st, id); !strings.Contains(p, "10.0.5.0/24") {
		t.Fatalf("not persisted: %q", p)
	}
	if err := d.SetPrivateRoutes(t.Context(), id, sandbox.PrivateRoutes{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Routes[id]; ok {
		t.Fatal("clearing left routes on the runtime")
	}
}

func TestSetPrivateRoutesRefusedWithoutCapability(t *testing.T) {
	d, _, _ := newRoutesDaemon(t, false)
	id := createRunning(t, d).ID
	if err := d.SetPrivateRoutes(t.Context(), id, corpRoutes); !errors.Is(err, ErrPrivateRoutesUnsupported) {
		t.Fatalf("got %v", err)
	}
	if err := d.SetPrivateRoutes(t.Context(), id, sandbox.PrivateRoutes{}); err != nil {
		t.Fatalf("clearing on a node without routes: %v", err)
	}
	if err := d.SetPrivateRoutes(t.Context(), id, sandbox.PrivateRoutes{Subnets: []string{"10.128.0.0/16"}}); err == nil {
		t.Fatal("a platform range was accepted")
	}
}

// Static routes come back from the row after a pause; learned addresses do not.
func TestResumeRepushesPrivateRoutes(t *testing.T) {
	d, fake, st := newRoutesDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.Pause(t.Context(), id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// The control plane writes the row while the sandbox is paused.
	if err := st.SetSandboxPrivateRoutes(t.Context(), id, `{"subnets":["10.0.5.0/24"],"dns":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := d.Resume(t.Context(), id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := fake.Routes[id]; len(got.Subnets) != 1 {
		t.Fatalf("after resume: %+v", got)
	}
	if !strings.Contains(storedRoutes(t, st, id), "10.0.5.0/24") {
		t.Fatal("the node's save clobbered the control plane's routes")
	}
}

func TestCheckpointRestoreRepushesPrivateRoutes(t *testing.T) {
	d, fake, _ := newRoutesDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.SetPrivateRoutes(t.Context(), id, corpRoutes); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Checkpoint(t.Context(), id, "c1"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	fake.Routes = nil
	if _, err := d.RestoreCheckpoint(t.Context(), id, "c1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fake.Routes[id]; len(got.Subnets) != 1 {
		t.Fatalf("after checkpoint restore: %+v", got)
	}
}

// A sandbox moved onto this node brings its stored routes.
func TestMovedInCreateAppliesPrivateRoutes(t *testing.T) {
	d, fake, st := newRoutesDaemon(t, true)
	const id = "sb-1790000000000-0123abce"
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{ID: id, OrgID: "o", Template: "base", State: "running", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSandboxPrivateRoutes(t.Context(), id, `{"subnets":["10.0.5.0/24"],"dns":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(t.Context(), CreateSpec{Template: "base", OrgID: "o", ID: id}); err != nil {
		t.Fatalf("moved-in create: %v", err)
	}
	if got := fake.Routes[id]; len(got.Subnets) != 1 {
		t.Fatalf("after move: %+v", got)
	}
}

func TestForkDoesNotCopyPrivateRoutes(t *testing.T) {
	d, fake, st := newRoutesDaemon(t, true)
	id := createRunning(t, d).ID
	if err := d.SetPrivateRoutes(t.Context(), id, corpRoutes); err != nil {
		t.Fatal(err)
	}
	res, err := d.Fork(t.Context(), id, ForkSpec{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if _, ok := fake.Routes[res.Child.ID]; ok || strings.Contains(storedRoutes(t, st, res.Child.ID), "10.0.5.0/24") {
		t.Fatal("fork inherited private routes")
	}
}
