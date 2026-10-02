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
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
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

	info, ok := d.Get(id)
	if !ok || info.State != StateRunning {
		t.Fatalf("present %v, state %q; want the rehomed sandbox running", ok, info.State)
	}
	if _, err := os.Stat(filepath.Join(d.snapshotDir(info.Template), "rootfs.squashfs")); err != nil {
		t.Fatalf("the capture the rehomed sandbox runs from is gone: %v", err)
	}
}

// writeCapture lays down tmpl as a fetched or staged capture and returns its rootfs.
func writeCapture(t *testing.T, d *Daemon, tmpl, content string) string {
	t.Helper()
	rootfs := filepath.Join(d.snapshotDir(tmpl), "rootfs.squashfs")
	if err := os.MkdirAll(filepath.Dir(rootfs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return rootfs
}

func newCaptureDaemon(t *testing.T, cfg Config) (*Daemon, *fakePersister) {
	t.Helper()
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })
	fp := newFakePersister(nil)
	if cfg.Runtime == nil {
		cfg.Runtime = runtimetest.NewFake()
	}
	if cfg.Store == nil {
		cfg.Store = fp
	}
	cfg.IdleTimeout, cfg.HardTTL = time.Hour, time.Hour
	return New(cfg), fp
}

// moveCapture is a template a move of id lands it on.
func moveCapture(id string) string {
	return store.SnapshotTemplateID("o", store.MoveSnapshotName(id)+"-0123abcd")
}

// A rehome's or a move's capture is a template of the sandbox it moved: kept
// after its last user's destroy, every rehome and move filled the node's disk.
// A named snapshot the owner saved outlives the sandbox.
func TestDestroyRemovesTheCaptureItsLastUserRanOn(t *testing.T) {
	d, _ := newCaptureDaemon(t, Config{})
	ctx := context.Background()
	named := store.SnapshotTemplateID("o", "before-upgrade")
	for _, tmpl := range []string{rehomeTemplate("sb-1790000000000-0123abcd"), moveCapture("sb-1790000000000-0123abcd"), named} {
		writeCapture(t, d, tmpl, tmpl)
		sb, err := d.Create(ctx, CreateSpec{Template: tmpl, OrgID: "o"})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Destroy(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(d.snapshotDir(tmpl))
		if tmpl == named && err != nil {
			t.Errorf("the named snapshot %s went with the sandbox: %v", named, err)
		}
		if tmpl != named && !os.IsNotExist(err) {
			t.Errorf("%s survived its last user's destroy: %v", tmpl, err)
		}
	}
}

// A fork runs on its parent's template, so a fork of a rehomed or moved
// sandbox runs on that sandbox's capture: destroying the parent removed it,
// and the fork's next resume or restart had no root image to boot.
func TestDestroyKeepsACaptureAForkStillRunsOn(t *testing.T) {
	d, _ := newCaptureDaemon(t, Config{})
	ctx := context.Background()
	for _, tmpl := range []string{rehomeTemplate("sb-1790000000000-0123abcd"), moveCapture("sb-1790000000000-0123abcd")} {
		rootfs := writeCapture(t, d, tmpl, "root")
		parent, err := d.Create(ctx, CreateSpec{Template: tmpl, OrgID: "o"})
		if err != nil {
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

// A sandbox rehomed back to a node where a fork of it still runs on its
// earlier capture: the rehome replaced that capture under the fork.
func TestARehomeBackLeavesTheCaptureAForkRunsOn(t *testing.T) {
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	archive := &BlobArchive{Store: blobs}
	fp := recordFake{newFakePersister(nil)}
	d, _ := newCaptureDaemon(t, Config{Store: fp, SnapshotArchive: archive})
	ctx := context.Background()
	const id = "sb-1790000000000-0123abcd"
	older := rehomeTemplate(id)
	rootfs := writeCapture(t, d, older, "older")
	parent, err := d.Create(ctx, CreateSpec{Template: older, OrgID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	fork, err := d.Fork(ctx, parent.ID, ForkSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}

	capture := t.TempDir()
	for _, name := range []string{"mem", "state", "rootfs.squashfs"} {
		if err := os.WriteFile(filepath.Join(capture, name), []byte("newer"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.PutPause(ctx, id, capture, "p2"); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	fp.saved[id] = PersistedSandbox{ID: id, OrgID: "o", Template: older, State: "paused", PauseArchive: "p2", CreatedAt: 100}
	fp.mu.Unlock()
	if err := d.Rehome(ctx, id, "o"); err != nil {
		t.Fatal(err)
	}

	info, _ := d.Get(id)
	if info.Template == older {
		t.Fatalf("the rehome landed on %s, the capture the fork runs on", older)
	}
	if got, err := os.ReadFile(rootfs); err != nil || string(got) != "older" {
		t.Fatalf("the fork's capture after the rehome = %q, %v; want it untouched", got, err)
	}
	if err := d.Pause(ctx, fork.Child.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Resume(ctx, fork.Child.ID); err != nil {
		t.Fatalf("the fork does not resume after the rehome: %v", err)
	}
}

// gatedBoot holds every Boot until released.
type gatedBoot struct {
	*runtimetest.Fake
	booting, release chan struct{}
}

func (g *gatedBoot) Boot(ctx context.Context, spec rt.BootSpec) (rt.Instance, error) {
	close(g.booting)
	<-g.release
	return g.Fake.Boot(ctx, spec)
}

// A create booting from a capture is not in d.boxes until its boot returns: a
// destroy of the capture's last loaded user in that window removed it.
func TestADestroyRacingAStartingCreateKeepsItsTemplate(t *testing.T) {
	fake := runtimetest.NewFake()
	d, _ := newCaptureDaemon(t, Config{Runtime: fake})
	ctx := context.Background()
	tmpl := moveCapture("sb-1790000000000-0123abcd")
	rootfs := writeCapture(t, d, tmpl, "root")
	parent, err := d.Create(ctx, CreateSpec{Template: tmpl, OrgID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	gate := &gatedBoot{Fake: fake, booting: make(chan struct{}), release: make(chan struct{})}
	d.cfg.Runtime = gate
	created := make(chan error, 1)
	go func() {
		_, err := d.Create(ctx, CreateSpec{Template: tmpl, OrgID: "o"})
		created <- err
	}()
	<-gate.booting

	if err := d.Destroy(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Fatalf("the capture a create was booting from went with the destroy: %v", err)
	}
}

// Until a restore has loaded every record, a sandbox on a capture may not be
// in d.boxes yet.
func TestADestroyDuringAnUnfinishedRestoreRemovesNoCapture(t *testing.T) {
	d, _ := newCaptureDaemon(t, Config{})
	ctx := context.Background()
	tmpl := moveCapture("sb-1790000000000-0123abcd")
	rootfs := writeCapture(t, d, tmpl, "root")
	sb, err := d.Create(ctx, CreateSpec{Template: tmpl, OrgID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.gone = map[string]bool{} // as restoreFromStore leaves it on an unfinished pass
	d.mu.Unlock()

	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Fatalf("a capture was removed while a restore was unfinished: %v", err)
	}
}

// A stored record this node has not loaded (another node's, or one a restore
// left) still runs on its capture.
func TestAStoredRecordKeepsItsCapture(t *testing.T) {
	d, fp := newCaptureDaemon(t, Config{})
	ctx := context.Background()
	tmpl := moveCapture("sb-1790000000000-0123abcd")
	rootfs := writeCapture(t, d, tmpl, "root")
	sb, err := d.Create(ctx, CreateSpec{Template: tmpl, OrgID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	fp.saved["sb-1790000000001-0123abcd"] = PersistedSandbox{ID: "sb-1790000000001-0123abcd", OrgID: "o", Template: tmpl, State: "paused"}
	fp.mu.Unlock()

	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Fatalf("the capture a stored record names went with the destroy: %v", err)
	}
}

// slowStop is an instance whose teardown waits to be let go.
type slowStop struct {
	rt.Instance
	stopping, release chan struct{}
}

func (s *slowStop) Stop(ctx context.Context) error {
	close(s.stopping)
	<-s.release
	return s.Instance.Stop(ctx)
}

// gatedCreate holds the second record read, a rehome's Create, until released:
// the rehome's capture is in place and nothing runs on it yet.
type gatedCreate struct {
	recordFake
	mu            sync.Mutex
	reads         int
	held, release chan struct{}
}

func (g *gatedCreate) SandboxRecord(ctx context.Context, id string) (PersistedSandbox, bool, error) {
	g.mu.Lock()
	n := g.reads
	g.reads++
	g.mu.Unlock()
	if n == 1 {
		close(g.held)
		<-g.release
	}
	return g.recordFake.SandboxRecord(ctx, id)
}

// A destroy forgets the sandbox before its teardown: a rehome of the same id
// in that window staged a capture the teardown then removed, under the
// sandbox the rehome went on to boot.
func TestARehomeDuringADestroyKeepsItsCapture(t *testing.T) {
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fp := &gatedCreate{recordFake: recordFake{newFakePersister(nil)}, held: make(chan struct{}), release: make(chan struct{})}
	archive := &BlobArchive{Store: blobs}
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, SnapshotArchive: archive, IdleTimeout: time.Hour, HardTTL: time.Hour})
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	capture := t.TempDir()
	for _, name := range []string{"mem", "state", "rootfs.squashfs"} {
		if err := os.WriteFile(filepath.Join(capture, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.PutPause(ctx, sb.ID, capture, "p1"); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	fp.saved[sb.ID] = PersistedSandbox{ID: sb.ID, OrgID: "o", Template: "base", State: "paused", PauseArchive: "p1", CreatedAt: 100}
	fp.reads = 0
	fp.mu.Unlock()
	slow := &slowStop{stopping: make(chan struct{}), release: make(chan struct{})}
	d.mu.Lock()
	slow.Instance = d.boxes[sb.ID].inst
	d.boxes[sb.ID].inst = slow
	d.mu.Unlock()

	destroyed, rehomed := make(chan error, 1), make(chan error, 1)
	go func() { destroyed <- d.Destroy(ctx, sb.ID) }()
	<-slow.stopping
	go func() { rehomed <- d.Rehome(ctx, sb.ID, "o") }()
	select {
	case <-fp.held: // the rehome raced into the teardown
	case <-time.After(200 * time.Millisecond): // it waits for the teardown
	}
	close(slow.release)
	if err := <-destroyed; err != nil {
		t.Fatal(err)
	}
	close(fp.release)
	<-rehomed

	if info, ok := d.Get(sb.ID); ok && info.State == StateRunning {
		if _, err := os.Stat(filepath.Join(d.snapshotDir(info.Template), "rootfs.squashfs")); err != nil {
			t.Fatalf("the rehomed sandbox runs on a capture the destroy removed: %v", err)
		}
	}
}
