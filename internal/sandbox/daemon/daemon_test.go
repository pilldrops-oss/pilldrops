package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func newTestDaemon(t *testing.T) (*Daemon, *runtimetest.Fake) {
	t.Helper()
	fake := runtimetest.NewFake()
	d := New(Config{
		Runtime:     fake,
		IdleTimeout: time.Minute,
		HardTTL:     time.Hour,
	})
	return d, fake
}

func TestCreateExecDestroy(t *testing.T) {
	d, fake := newTestDaemon(t)
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.State != StateRunning {
		t.Fatalf("state = %s, want running", sb.State)
	}

	if _, err := d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "echo hi"}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(fake.ExecLog(sb.ID)) != 1 {
		t.Fatalf("exec not forwarded to runtime")
	}

	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, ok := d.Get(sb.ID); ok {
		t.Fatalf("sandbox still present after destroy")
	}
}

func TestExecDetachReturnsPid(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "sleep 100", Detach: true})
	if err != nil {
		t.Fatalf("detached exec: %v", err)
	}
	if res.Pid == 0 {
		t.Fatalf("detached exec returned no pid")
	}
}

func TestServerCrashScannerEmitsOnEdge(t *testing.T) {
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{Runtime: fake, Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A real listener on a free port; the fake's ListenTarget resolves to it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	d.mu.Lock()
	d.boxes[sb.ID].StartCmd = "python3 app.py"
	d.boxes[sb.ID].PublicPort = port
	d.mu.Unlock()

	d.scanServerCrashes(ctx)
	if n := fp.countEvents("crash"); n != 0 {
		t.Fatalf("crash while up = %d, want 0", n)
	}

	ln.Close()
	d.scanServerCrashes(ctx)
	if n := fp.countEvents("crash"); n != 1 {
		t.Fatalf("crash after down = %d, want 1", n)
	}

	// Edge-triggered: still down, no duplicate.
	d.scanServerCrashes(ctx)
	if n := fp.countEvents("crash"); n != 1 {
		t.Fatalf("crash duplicated while still down = %d, want 1", n)
	}
}

func TestOOMScannerEmitsEventOnRise(t *testing.T) {
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{Runtime: fake, Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d.scanOOMKills(ctx)
	if n := fp.countEvents("oom"); n != 0 {
		t.Fatalf("oom events before any kill = %d, want 0", n)
	}

	fake.SetOOMKills(sb.ID, 1)
	d.scanOOMKills(ctx)
	if n := fp.countEvents("oom"); n != 1 {
		t.Fatalf("oom events after first kill = %d, want 1", n)
	}

	d.scanOOMKills(ctx)
	if n := fp.countEvents("oom"); n != 1 {
		t.Fatalf("oom events after unchanged count = %d, want 1 (dup emitted)", n)
	}

	fake.SetOOMKills(sb.ID, 3)
	d.scanOOMKills(ctx)
	if n := fp.countEvents("oom"); n != 2 {
		t.Fatalf("oom events after second rise = %d, want 2", n)
	}
}

// An exec on a paused sandbox wakes it: no resume call, the exec runs, and the
// sandbox is running afterwards. A cancelled caller is refused, not hung.
func TestExecWakesPausedSandbox(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, _ := d.Create(ctx, CreateSpec{Template: "python-3.11"})

	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got, _ := d.Get(sb.ID)
	if got.State != StatePaused {
		t.Fatalf("state = %s, want paused", got.State)
	}
	if _, err := d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "echo x"}); err != nil {
		t.Fatalf("exec while paused = %v, want a wake and nil", err)
	}
	if got, _ = d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state after exec = %s, want running", got.State)
	}
	// A second exec finds it running and does not resume again.
	if _, err := d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "echo x"}); err != nil {
		t.Fatalf("exec after wake: %v", err)
	}

	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("second pause: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.Exec(cancelled, sb.ID, rt.ExecSpec{Cmd: "echo x"}); err == nil {
		t.Fatal("exec with a dead context on a paused sandbox returned nil, want the context error")
	}
}

func TestExecUnknownSandbox(t *testing.T) {
	d, _ := newTestDaemon(t)
	if _, err := d.Exec(context.Background(), "nope", rt.ExecSpec{Cmd: "x"}); err != rt.ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestProcsAndSignal(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})

	procs, err := d.Procs(sb.ID)
	if err != nil {
		t.Fatalf("procs: %v", err)
	}
	if len(procs) == 0 {
		t.Fatalf("expected at least one proc from the fake runtime")
	}
	if err := d.Signal(sb.ID, procs[0].PID, 15); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if _, err := d.Procs("nope"); err == nil {
		t.Fatalf("expected ErrNotFound for unknown sandbox")
	}
}

func TestDestroyRecordsHistory(t *testing.T) {
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: fake, Store: fp, NodeID: "node-test",
		SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "acme"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	fp.mu.Lock()
	h, ok := fp.history[sb.ID]
	fp.mu.Unlock()
	if !ok {
		t.Fatal("Destroy did not record history")
	}
	if h.Template != "python-3.11" || h.OrgID != "acme" || h.ExitState != "destroyed" {
		t.Fatalf("history record wrong: %+v", h)
	}
	if h.CreatedAt == 0 || h.DestroyedAt == 0 {
		t.Fatalf("history times not set: %+v", h)
	}
	if h.Node != "node-test" {
		t.Fatalf("history node = %q, want node-test", h.Node)
	}
}

// snapshotDeleterFake adds rt.SnapshotDeleter to the fake and counts calls per sandbox.
type snapshotDeleterFake struct {
	*runtimetest.Fake
	mu    sync.Mutex
	calls map[string]int
}

func newSnapshotDeleterFake() *snapshotDeleterFake {
	return &snapshotDeleterFake{Fake: runtimetest.NewFake(), calls: map[string]int{}}
}

func (f *snapshotDeleterFake) DeleteSnapshot(_ context.Context, sandboxID string, _ rt.SnapshotRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[sandboxID]++
	return nil
}

func (f *snapshotDeleterFake) deleteCalls(sandboxID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[sandboxID]
}

// Pause nils sb.inst, so the paused branch is the one an inst-guarded delete would skip.
func TestDestroy_DeletesSnapshot_RunningAndPaused(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "running"
		if paused {
			name = "paused_inst_nil"
		}
		t.Run(name, func(t *testing.T) {
			fake := newSnapshotDeleterFake()
			d := New(Config{
				Runtime:     fake,
				IdleTimeout: time.Hour,
				HardTTL:     time.Hour,
			})
			ctx := context.Background()
			sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if paused {
				if err := d.Pause(ctx, sb.ID); err != nil {
					t.Fatalf("pause: %v", err)
				}
			}
			if err := d.Destroy(ctx, sb.ID); err != nil {
				t.Fatalf("destroy: %v", err)
			}
			if calls := fake.deleteCalls(sb.ID); calls != 1 {
				t.Fatalf("DeleteSnapshot calls=%d, want 1 (paused=%v)", calls, paused)
			}
		})
	}
}

type fakePersister struct {
	mu      sync.Mutex
	preload []PersistedSandbox
	saved   map[string]PersistedSandbox
	deleted map[string]bool
	history map[string]SandboxHistory
	events  []string
	balance float64
	// billingOff makes Balance report ok=false, as with no billing configured.
	billingOff bool
	// balanceErr makes the ledger read fail.
	balanceErr error
	// used accumulates every flushed usage delta.
	used Usage
	// usageWrites counts store round trips.
	usageWrites int
	// balanceReads counts ledger reads.
	balanceReads int
}

func newFakePersister(preload []PersistedSandbox) *fakePersister {
	return &fakePersister{
		preload: preload,
		saved:   map[string]PersistedSandbox{},
		deleted: map[string]bool{},
		history: map[string]SandboxHistory{},
		balance: 1, // funded by default; tests set balance<=0 to exercise out-of-credits
	}
}

func (f *fakePersister) SaveSandbox(ctx context.Context, rec PersistedSandbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[rec.ID] = rec
	return nil
}

func (f *fakePersister) DeleteSandbox(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[id] = true
	return nil
}

func (f *fakePersister) RecordUsage(ctx context.Context, _, _ string, u Usage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageWrites++
	f.used.VCPUSeconds += u.VCPUSeconds
	f.used.RAMGBSeconds += u.RAMGBSeconds
	f.used.StorageGBSeconds += u.StorageGBSeconds
	f.used.RunSeconds += u.RunSeconds
	return nil
}

// billed returns the accumulated usage flushed so far.
func (f *fakePersister) billed() Usage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used
}
func (f *fakePersister) RecordHistory(ctx context.Context, r SandboxHistory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history[r.ID] = r
	return nil
}

func (f *fakePersister) RecordEvent(ctx context.Context, orgID, sandboxID, event, template string, activeAfter int, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakePersister) countEvents(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e == kind {
			n++
		}
	}
	return n
}

func (f *fakePersister) LoadSandboxes(ctx context.Context) ([]PersistedSandbox, error) {
	return f.preload, nil
}

func (f *fakePersister) Balance(ctx context.Context, orgID string) (float64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balanceReads++
	if f.balanceErr != nil {
		return 0, true, f.balanceErr
	}
	return f.balance, !f.billingOff, nil
}

func TestShutdownPausesRunningSandboxes(t *testing.T) {
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: fake, Store: fp,
		SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	d.Shutdown(ctx)

	got, ok := d.Get(sb.ID)
	if !ok || got.State != StatePaused {
		t.Fatalf("state after shutdown = %v (ok=%v), want paused", got.State, ok)
	}
	fp.mu.Lock()
	rec := fp.saved[sb.ID]
	fp.mu.Unlock()
	if rec.State != string(StatePaused) {
		t.Errorf("persisted state = %q, want paused", rec.State)
	}
	if rec.Snapshot == "" {
		t.Errorf("persisted record has no snapshot ref")
	}
	if rec.StartCmd != "python3 app.py" || rec.PublicPort != 8000 {
		t.Errorf("persisted app config = (%q, %d), want (python3 app.py, 8000)", rec.StartCmd, rec.PublicPort)
	}
}

// Paused or running-with-snapshot records come back paused; unrecoverable or expired ones are dropped.
func TestRestoreOnStart(t *testing.T) {
	now := time.Now().Unix()
	preload := []PersistedSandbox{
		{ID: "sb_paused", Template: "python-3.11", OrgID: "default", State: "paused",
			Snapshot: "/snap/sb_paused", StartCmd: "python3 app.py", PublicPort: 8000,
			CreatedAt: now - 100, ExpiresAt: now + 3600},
		{ID: "sb_crashed_snap", Template: "node-20", OrgID: "default", State: "running",
			Snapshot: "/snap/sb_crashed_snap", CreatedAt: now - 100, ExpiresAt: now + 3600},
		{ID: "sb_crashed_nosnap", Template: "node-20", OrgID: "default", State: "running",
			Snapshot: "", CreatedAt: now - 100, ExpiresAt: now + 3600},
		{ID: "sb_expired", Template: "node-20", OrgID: "default", State: "paused",
			Snapshot: "/snap/sb_expired", CreatedAt: now - 7200, ExpiresAt: now - 60},
	}
	fake := runtimetest.NewFake()
	fp := newFakePersister(preload)
	d := New(Config{
		Runtime: fake, Store: fp,
		SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()
	ctx := context.Background()

	if got, ok := d.Get("sb_paused"); !ok || got.State != StatePaused {
		t.Fatalf("sb_paused = %v (ok=%v), want restored as paused", got.State, ok)
	}
	if got, ok := d.Get("sb_crashed_snap"); !ok || got.State != StatePaused {
		t.Errorf("sb_crashed_snap = %v (ok=%v), want restored as paused from snapshot", got.State, ok)
	}
	if _, ok := d.Get("sb_crashed_nosnap"); ok {
		t.Errorf("sb_crashed_nosnap restored, want dropped (no snapshot to restore from)")
	}
	if _, ok := d.Get("sb_expired"); ok {
		t.Errorf("sb_expired restored, want dropped (past hard TTL)")
	}
	fp.mu.Lock()
	droppedNoSnap, droppedExpired := fp.deleted["sb_crashed_nosnap"], fp.deleted["sb_expired"]
	fp.mu.Unlock()
	if !droppedNoSnap || !droppedExpired {
		t.Errorf("dropped records not deleted from store: nosnap=%v expired=%v", droppedNoSnap, droppedExpired)
	}

	if err := d.Resume(ctx, "sb_paused"); err != nil {
		t.Fatalf("resume restored sandbox: %v", err)
	}
	if got, _ := d.Get("sb_paused"); got.State != StateRunning {
		t.Fatalf("state after resume = %v, want running", got.State)
	}
	replayed := false
	for _, cmd := range fake.ServerLog("sb_paused") {
		if strings.Contains(cmd, "python3 app.py") {
			replayed = true
		}
	}
	if !replayed {
		t.Errorf("start command not replayed on resume; server log = %v", fake.ServerLog("sb_paused"))
	}
	if _, err := d.PreviewTarget(ctx, "sb_paused", 9999, "default"); err == nil {
		t.Errorf("preview on unpublished port succeeded, want port restriction restored")
	}
}

func TestResumeReplaysServerEnv(t *testing.T) {
	d, fake := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	env := map[string]string{"GREETING": "hello"}
	if err := d.StartServer(ctx, sb.ID, "run", 8000, env); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := d.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := fake.LastServerEnv(sb.ID)
	if got["GREETING"] != "hello" {
		t.Fatalf("resume replayed env = %v, want GREETING=hello", got)
	}
}

func TestLifecycleTransitionsAreLogged(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	fake := runtimetest.NewFake()
	d := New(Config{
		Runtime:     fake,
		IdleTimeout: time.Minute,
		HardTTL:     time.Hour,
		Logger: slog.New(captureHandler{func(msg string) {
			mu.Lock()
			lines = append(lines, msg)
			mu.Unlock()
		}}),
	})
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := d.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	// Each line must carry the sandbox id as an attribute, not only in the message.
	for _, want := range []string{"created", "paused", "resumed", "destroyed"} {
		found := false
		mu.Lock()
		for _, l := range lines {
			if strings.Contains(l, want) && strings.Contains(l, "sandbox_id="+sb.ID) {
				found = true
				break
			}
		}
		mu.Unlock()
		if !found {
			t.Errorf("no %q log line carrying sandbox_id=%s; got %v", want, sb.ID, lines)
		}
	}
}

func TestRestoreReplaysPersistedEnv(t *testing.T) {
	now := time.Now().Unix()
	fake := runtimetest.NewFake()
	fp := newFakePersister([]PersistedSandbox{
		{ID: "sb_env", Template: "python-3.11", OrgID: "default", State: "paused",
			Snapshot: "/snap/sb_env", StartCmd: "run", PublicPort: 8000,
			Env:       map[string]string{"TAG": "prod"},
			CreatedAt: now - 100, ExpiresAt: now + 3600},
	})
	d := New(Config{
		Runtime: fake, Store: fp,
		SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()

	if err := d.Resume(context.Background(), "sb_env"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := fake.LastServerEnv("sb_env"); got["TAG"] != "prod" {
		t.Fatalf("restored resume env = %v, want TAG=prod", got)
	}
}

// captureHandler renders each record as "message key=value ..." so tests can
// assert on attributes, not just the message.
type captureHandler struct{ fn func(string) }

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	h.fn(b.String())
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// A memory-snapshot resume brings the app back with the rest of the guest, so
// relaunching the start command would leave two copies of it running. Nothing
// refuses the second one: StartServer is a backgrounded shell.
func TestMemoryResumeDoesNotStartTheAppTwice(t *testing.T) {
	ctx := context.Background()
	d, fake := newTestDaemon(t)
	fake.MemoryRestore = true
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.StartServer(ctx, sb.ID, "serve.sh", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := d.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := fake.ServerLog(sb.ID); len(got) != 0 {
		t.Fatalf("a resume from a memory image relaunched the start command %v; "+
			"the restored guest is already running it", got)
	}

	// The disk tier is the other half: with no memory image the processes are
	// gone and the resume has to bring the app back.
	d2, disk := newTestDaemon(t)
	sb2, err := d2.Create(ctx, CreateSpec{Template: "python-3.11"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d2.StartServer(ctx, sb2.ID, "serve.sh", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if err := d2.Pause(ctx, sb2.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := d2.Resume(ctx, sb2.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := disk.ServerLog(sb2.ID); len(got) != 1 || got[0] != "serve.sh" {
		t.Fatalf("a resume with no memory image left the app unstarted: %v", got)
	}
}
