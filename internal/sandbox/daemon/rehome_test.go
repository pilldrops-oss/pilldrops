package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// A rehome whose boot fails must not leave its capture behind as a template:
// on the rig /v1/templates listed snap-rehome-<id> to the tenant.
func TestAFailedRehomeLeavesNoTemplate(t *testing.T) {
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const id = "sb-1790000000000-0123abcd"
	capture := t.TempDir()
	for _, name := range []string{"mem", "state", "rootfs.squashfs"} {
		if err := os.WriteFile(filepath.Join(capture, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive := &BlobArchive{Store: blobs}
	if err := archive.PutPause(ctx, id, capture, "p1"); err != nil {
		t.Fatal(err)
	}
	fp := recordFake{newFakePersister(nil)}
	fp.saved[id] = PersistedSandbox{ID: id, OrgID: "o", Template: "base", State: "paused", PauseArchive: "p1", CreatedAt: 100}
	fake := runtimetest.NewFake()
	fake.BootErr = errors.New("Failed to set all KVM MSRs for this vCPU")
	d := New(Config{Runtime: fake, Store: fp, SnapshotArchive: archive, IdleTimeout: time.Minute, HardTTL: time.Hour})

	if err := d.Rehome(ctx, id, "o"); err == nil {
		t.Fatal("a rehome whose boot failed reported success")
	}
	if ids := templates.PublishedIDs(); len(ids) != 0 {
		t.Fatalf("templates left after the failed rehome: %v", ids)
	}
	if ok, err := archive.Get(ctx, id, filepath.Join(t.TempDir(), "again")); !ok || err != nil {
		t.Fatalf("the archive was lost with the failed rehome: %v, %v", ok, err)
	}
}

// gatedBlobs holds the first archive read (the first rehome's) until released,
// and a read arriving while it is held (a second rehome's) until firstDone.
type gatedBlobs struct {
	BlobStore
	mu                       sync.Mutex
	reads                    int
	held                     bool
	firstHeld, secondArrived chan struct{}
	release, firstDone       chan struct{}
}

func (g *gatedBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	g.mu.Lock()
	n, held := g.reads, g.held
	g.reads++
	if n == 0 {
		g.held = true
	}
	g.mu.Unlock()
	switch {
	case n == 0:
		close(g.firstHeld)
		<-g.release
		g.mu.Lock()
		g.held = false
		g.mu.Unlock()
	case held:
		close(g.secondArrived)
		<-g.firstDone
	}
	return g.BlobStore.Get(ctx, key)
}

// Two rehomes of one sandbox at once: the second replaced, then on its Create
// failing with "already runs on this node" removed, the capture the first runs from.
func TestAConcurrentRehomeLeavesTheLiveSandboxsCapture(t *testing.T) {
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const id = "sb-1790000000000-0123abcd"
	capture := t.TempDir()
	// mem.lazy: its memory is served from the archive, so the first rehome
	// keeps the archive for the second to fetch.
	for _, name := range []string{"mem.lazy", "state", "rootfs.squashfs"} {
		if err := os.WriteFile(filepath.Join(capture, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&BlobArchive{Store: blobs}).PutPause(ctx, id, capture, "p1"); err != nil {
		t.Fatal(err)
	}
	gate := &gatedBlobs{BlobStore: blobs, firstHeld: make(chan struct{}), secondArrived: make(chan struct{}),
		release: make(chan struct{}), firstDone: make(chan struct{})}
	fp := recordFake{newFakePersister(nil)}
	fp.saved[id] = PersistedSandbox{ID: id, OrgID: "o", Template: "base", State: "paused", PauseArchive: "p1", CreatedAt: 100}
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, SnapshotArchive: &BlobArchive{Store: gate},
		IdleTimeout: time.Minute, HardTTL: time.Hour})

	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- d.Rehome(ctx, id, "o") }()
	<-gate.firstHeld
	go func() { second <- d.Rehome(ctx, id, "o") }()
	select {
	case <-gate.secondArrived:
	case <-time.After(200 * time.Millisecond): // the second waits for the first instead
	}
	close(gate.release)
	if err := <-first; err != nil {
		t.Fatalf("first rehome: %v", err)
	}
	close(gate.firstDone)
	<-second

	if info, ok := d.Get(id); !ok || info.State != StateRunning {
		t.Fatalf("present %v, state %q; want the rehomed sandbox running", ok, info.State)
	}
	if _, err := os.Stat(filepath.Join(templates.Dir(), "snap-rehome-"+id, "rootfs.squashfs")); err != nil {
		t.Fatalf("the capture the rehomed sandbox runs from is gone: %v", err)
	}
}

// A rehome's or a move's capture is a template of one sandbox: kept after its
// destroy, every rehome and move filled the node's disk. A named snapshot the
// owner saved outlives the sandbox.
func TestDestroyRemovesTheSandboxsCaptureTemplates(t *testing.T) {
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })
	d := New(Config{Runtime: runtimetest.NewFake(), Store: newFakePersister(nil), IdleTimeout: time.Minute, HardTTL: time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	rehome := rehomeTemplate(sb.ID)
	move := moveTemplate("o", sb.ID)
	named := store.SnapshotTemplateID("o", "before-upgrade")
	for _, tmpl := range []string{rehome, move, named} {
		if err := os.MkdirAll(d.snapshotDir(tmpl), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d.snapshotDir(tmpl), "rootfs.squashfs"), []byte(tmpl), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range []string{rehome, move} {
		if _, err := os.Stat(d.snapshotDir(tmpl)); !os.IsNotExist(err) {
			t.Errorf("%s survived its sandbox's destroy: %v", tmpl, err)
		}
	}
	if _, err := os.Stat(filepath.Join(d.snapshotDir(named), "rootfs.squashfs")); err != nil {
		t.Errorf("the named snapshot %s went with the sandbox: %v", named, err)
	}
}

// A fork runs on its parent's template, so a fork of a rehomed or moved
// sandbox runs on that sandbox's capture: destroying the parent removed it,
// and the fork's next resume or restart had no root image to boot.
func TestDestroyKeepsACaptureAForkStillRunsOn(t *testing.T) {
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })
	d := New(Config{Runtime: runtimetest.NewFake(), Store: newFakePersister(nil), IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()
	for _, capture := range []func(id string) string{rehomeTemplate, func(id string) string { return moveTemplate("o", id) }} {
		parent, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "o"})
		if err != nil {
			t.Fatal(err)
		}
		tmpl := capture(parent.ID)
		d.mu.Lock()
		d.boxes[parent.ID].Template = tmpl // as a rehome or a move leaves it
		d.mu.Unlock()
		rootfs := filepath.Join(d.snapshotDir(tmpl), "rootfs.squashfs")
		if err := os.MkdirAll(filepath.Dir(rootfs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rootfs, []byte("root"), 0o600); err != nil {
			t.Fatal(err)
		}
		fork, err := d.Fork(ctx, parent.ID, ForkSpec{})
		if err != nil {
			t.Fatal(err)
		}

		if err := d.Destroy(ctx, parent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(rootfs); err != nil {
			t.Fatalf("%s: the capture the fork runs on went with its parent: %v", tmpl, err)
		}
		if err := d.Pause(ctx, fork.Child.ID); err != nil {
			t.Fatal(err)
		}
		if err := d.Resume(ctx, fork.Child.ID); err != nil {
			t.Fatalf("the fork does not resume after its parent's destroy: %v", err)
		}

		if err := d.Destroy(ctx, fork.Child.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(d.snapshotDir(tmpl)); !os.IsNotExist(err) {
			t.Errorf("%s outlived its last user: %v", tmpl, err)
		}
	}
}
