package firecracker

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/vm"
)

// setNetworks records the last membership push per vm id.
func (f *fakeBackend) SetNetworks(id string, n rt.Networks) (string, error) {
	if f.networks == nil {
		f.networks = map[string][]string{}
	}
	if len(n.Sets) == 0 {
		delete(f.networks, id)
		return "", nil
	}
	f.networks[id] = append([]string(nil), n.Sets...)
	return "10.128.0.2", nil
}

type fakeBackend struct {
	bootedFrom    string
	bootedEgress  *egress.Rules
	networks      map[string][]string
	execs         []string
	detachedExecs []string
	execOut       map[string]string
	files         map[string][]byte
	snapshotted   []string
	restored      []string
	discarded     []string
	checkpointed  []string
	ckptRestored  []string
	frozen        []string
	thawed        []string
	snapErr       error
	restoreErr    error
	// identities records every DeliverIdentity call as "<vmID>:<token>"; identityErr fails them.
	identities  []string
	identityErr error
	destroyed   []string
	// nodeAddr is the namespace's host-reachable "<ip>:6001"; "" is the shared bridge.
	nodeAddr string
}

func (f *fakeBackend) DeliverIdentity(id string, ident rt.Identity) error {
	f.identities = append(f.identities, id+":"+ident.Token)
	return f.identityErr
}

func newFake() *fakeBackend {
	return &fakeBackend{execOut: map[string]string{}, files: map[string][]byte{}}
}

func (f *fakeBackend) Create(_ context.Context, req vm.CreateRequest) (*vm.Sandbox, error) {
	return &vm.Sandbox{ID: "vm_123", Template: req.Template, VMIP: "192.168.30.7", NodeAddr: f.nodeAddr}, nil
}
func (f *fakeBackend) Exec(_ context.Context, _, cmd, _ string, _ time.Duration) (*envd.ExecResult, error) {
	f.execs = append(f.execs, cmd)
	out := ""
	for sub, o := range f.execOut {
		if strings.Contains(cmd, sub) {
			out = o
		}
	}
	return &envd.ExecResult{Stdout: []byte(out), ExitCode: 0}, nil
}
func (f *fakeBackend) ExecStream(_ context.Context, _, cmd, _ string, _ time.Duration, onFrame func(envd.StreamFrame)) error {
	f.execs = append(f.execs, cmd)
	out := ""
	for sub, o := range f.execOut {
		if strings.Contains(cmd, sub) {
			out = o
		}
	}
	if out != "" {
		onFrame(envd.StreamFrame{Stdout: []byte(out)})
	}
	onFrame(envd.StreamFrame{Exited: true, ExitCode: 0})
	return nil
}
func (f *fakeBackend) ExecDetached(_ context.Context, _, cmd, _ string, _ time.Duration) (*envd.ExecResult, error) {
	f.detachedExecs = append(f.detachedExecs, cmd)
	f.execs = append(f.execs, cmd)
	return &envd.ExecResult{ExitCode: 0}, nil
}

func TestExecStreamRelaysFrames(t *testing.T) {
	be := newFake()
	be.execOut["mycmd"] = "hello-stream"
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	se, ok := inst.(rt.StreamExecutor)
	if !ok {
		t.Fatal("fcInstance must implement rt.StreamExecutor (G-4)")
	}
	var out string
	var exited bool
	err := se.ExecStream(context.Background(), rt.ExecSpec{Cmd: "mycmd"}, func(fr rt.StreamFrame) {
		out += string(fr.Stdout)
		if fr.Exited {
			exited = true
		}
	})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if !strings.Contains(out, "hello-stream") {
		t.Fatalf("stream did not relay stdout, got %q", out)
	}
	if !exited {
		t.Fatal("stream never delivered a terminal (Exited) frame")
	}
}
func (f *fakeBackend) ReadFile(_ context.Context, _, path string) ([]byte, error) {
	return f.files[path], nil
}
func (f *fakeBackend) WriteFile(_ context.Context, _, path string, data []byte) error {
	f.files[path] = data
	return nil
}
func (f *fakeBackend) Destroy(_ context.Context, id string) error {
	f.destroyed = append(f.destroyed, id)
	return nil
}
func (f *fakeBackend) Alive(string) bool { return true }

func (f *fakeBackend) RecordOwner(string, string) error      { return nil }
func (f *fakeBackend) AdoptOwned(string) (*vm.Sandbox, bool) { return nil, false }
func (f *fakeBackend) KillUnadopted()                        {}

func (f *fakeBackend) SnapshotSandbox(_ context.Context, id, destDir string) error {
	f.snapshotted = append(f.snapshotted, id+"→"+destDir)
	return f.snapErr
}
func (f *fakeBackend) ForkSandbox(_ context.Context, parentID, childID string, _, _ int) (*vm.Sandbox, time.Duration, error) {
	return &vm.Sandbox{ID: childID, Template: "base", VMIP: "192.168.30.2", NodeAddr: f.nodeAddr}, 0, nil
}
func (f *fakeBackend) ForkFromSnapshot(_ context.Context, _, childID string, _, _ int) (*vm.Sandbox, error) {
	return &vm.Sandbox{ID: childID, Template: "base", VMIP: "192.168.30.2", NodeAddr: f.nodeAddr}, nil
}

// bootedFrom records the capture directory BootFromCapture was handed, so a
// test can assert the restore was pointed at the fetched build rather than at
// some path the adapter composed itself.
func (f *fakeBackend) BootFromCapture(_ context.Context, srcDir, childID string, _, _ int, eg *egress.Rules) (*vm.Sandbox, error) {
	f.bootedFrom = srcDir
	f.bootedEgress = eg
	return &vm.Sandbox{ID: childID, Template: "base", VMIP: "192.168.30.2", NodeAddr: f.nodeAddr}, nil
}

func (f *fakeBackend) DiscardSnapshot(srcDir string) error {
	f.discarded = append(f.discarded, srcDir)
	return nil
}
func (f *fakeBackend) FreezeVM(_ context.Context, id string) error {
	f.frozen = append(f.frozen, id)
	return nil
}
func (f *fakeBackend) ThawVM(_ context.Context, id string) error {
	f.thawed = append(f.thawed, id)
	return nil
}
func (f *fakeBackend) CheckpointSandbox(_ context.Context, id, dir string) (time.Duration, int64, error) {
	f.checkpointed = append(f.checkpointed, id+"@"+dir)
	return 42 * time.Millisecond, 1234, nil
}
func (f *fakeBackend) RestoreCheckpoint(_ context.Context, id, ckptDir string, _, _ int) (*vm.Sandbox, error) {
	f.ckptRestored = append(f.ckptRestored, id+"@"+ckptDir)
	return &vm.Sandbox{ID: id, Template: "base", VMIP: "192.168.30.2", NodeAddr: f.nodeAddr}, nil
}
func (f *fakeBackend) RestoreSandbox(_ context.Context, id, srcDir string, _, _ int) (*vm.Sandbox, error) {
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	f.restored = append(f.restored, id+"←"+srcDir)
	return &vm.Sandbox{ID: "vm_restored_" + id, VMIP: "192.168.30.9", NodeAddr: f.nodeAddr}, nil
}

// Boot and Fork hand the guest its identity; a failed delivery tears the VM
// down and fails the boot, and a resume (zero identity) delivers nothing.
func TestIdentityDeliveredOrBootRefused(t *testing.T) {
	t.Setenv("FC_MEM_SNAPSHOTS", "1")
	be := newFake()
	r := newWithBackend(be)
	ident := rt.Identity{Token: "pd_sb_x", APIURL: "https://api", SandboxID: "sb1"}
	inst, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "base"}, Identity: ident})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if len(be.identities) != 1 || !strings.HasSuffix(be.identities[0], ":pd_sb_x") {
		t.Fatalf("identities delivered = %v", be.identities)
	}
	if _, _, err := r.Fork(context.Background(), rt.ForkSource{Parent: inst}, rt.BootSpec{SandboxID: "sb2", Identity: rt.Identity{Token: "pd_sb_y"}}); err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(be.identities) != 2 || !strings.HasSuffix(be.identities[1], ":pd_sb_y") {
		t.Fatalf("fork identity = %v", be.identities)
	}
	if _, err := r.Restore(context.Background(), rt.SnapshotRef{Path: t.TempDir()}, rt.BootSpec{SandboxID: "sb1"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(be.identities) != 2 {
		t.Fatalf("a resume must not push: %v", be.identities)
	}

	be.identityErr = os.ErrPermission
	_, err = r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb3", Template: rt.Template{Name: "base"}, Identity: ident})
	if err == nil || !strings.Contains(err.Error(), "identity") || !strings.Contains(err.Error(), "sb3") {
		t.Fatalf("a failed delivery must fail the boot naming the sandbox and the identity; got %v", err)
	}
	if len(be.destroyed) != 1 {
		t.Fatalf("the VM must be torn down after a failed delivery; destroyed=%v", be.destroyed)
	}
}

func TestBootAndListenTarget(t *testing.T) {
	be := newFake()
	r := newWithBackend(be)
	if r.Name() != "firecracker" {
		t.Fatalf("name = %q", r.Name())
	}
	if !r.Capabilities().Isolation {
		t.Fatalf("firecracker must report isolation")
	}
	inst, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "python-3.11"}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	target, _ := inst.ListenTarget(8000)
	if target != "192.168.30.7:8000" {
		t.Fatalf("listen target = %q, want 192.168.30.7:8000", target)
	}
	if inst.(*fcInstance).ID() != "sb1" {
		t.Fatalf("ID() = %q, want sb1 (daemon id)", inst.(*fcInstance).ID())
	}
}

func TestExecMapsResult(t *testing.T) {
	be := newFake()
	be.execOut["echo hi"] = "hi\n"
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	res, err := inst.Exec(context.Background(), rt.ExecSpec{Cmd: "echo hi"})
	if err != nil || res.Stdout != "hi\n" || res.ExitCode != 0 {
		t.Fatalf("exec = %+v, %v", res, err)
	}
}

func TestStartServerBackgrounds(t *testing.T) {
	be := newFake()
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	if err := inst.StartServer(context.Background(), "python3 app.py", nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	last := be.execs[len(be.execs)-1]
	if !strings.HasSuffix(strings.TrimSpace(last), "&") {
		t.Fatalf("start command not backgrounded: %q", last)
	}
	if len(be.detachedExecs) != 1 {
		t.Fatalf("StartServer must go through ExecDetached, got %d detached execs", len(be.detachedExecs))
	}
	if !strings.Contains(last, "python3 app.py") {
		t.Fatalf("start command lost the app: %q", last)
	}
	if !strings.Contains(last, "PORT=8000") {
		t.Fatalf("start command must inject PORT: %q", last)
	}
	// Output must land where the /logs endpoint reads, not the old /tmp path.
	wantLog := path.Join(fcHomeDir, rt.AppLogPath)
	if !strings.Contains(last, wantLog) {
		t.Fatalf("start must capture output to %q (the /logs read path), got %q", wantLog, last)
	}
	if strings.Contains(last, "/tmp/sandboxd-app.log") {
		t.Fatalf("start still writes the stale /tmp path the logs reader ignores: %q", last)
	}
}

func TestExecForwardsEnv(t *testing.T) {
	be := newFake()
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	_, _ = inst.Exec(context.Background(), rt.ExecSpec{Cmd: "printenv FOO", Env: map[string]string{"FOO": "bar"}})
	last := be.execs[len(be.execs)-1]
	if !strings.Contains(last, "env ") || !strings.Contains(last, "FOO=bar") {
		t.Fatalf("exec did not forward env: %q", last)
	}
}

func TestFSListParsesFind(t *testing.T) {
	be := newFake()
	// The third name carries a tab and a newline.
	be.execOut["find"] = "f\t42\tapp.py\x00d\t4096\tsub\x00f\t7\tweird\tna\nme\x00"
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	entries, err := inst.FS().List(t.Context(), ".")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (%v)", len(entries), entries)
	}
	byName := map[string]rt.DirEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if byName["app.py"].IsDir || byName["app.py"].Size != 42 {
		t.Fatalf("app.py entry wrong: %+v", byName["app.py"])
	}
	if !byName["sub"].IsDir {
		t.Fatalf("sub should be a dir: %+v", byName["sub"])
	}
	if e, ok := byName["weird\tna\nme"]; !ok || e.Size != 7 {
		t.Fatalf("filename with tab/newline not parsed intact: %+v", byName)
	}
}

func TestFSMutatorsAndReadWrite(t *testing.T) {
	be := newFake()
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	fs := inst.FS()

	if err := fs.Write(t.Context(), "a.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if b, _ := fs.Read(t.Context(), "a.txt"); string(b) != "hi" {
		t.Fatalf("read = %q", b)
	}
	_ = fs.Mkdir(t.Context(), "d")
	_ = fs.Move(t.Context(), "a.txt", "b.txt")
	_ = fs.Remove(t.Context(), "b.txt")
	joined := strings.Join(be.execs, "\n")
	for _, want := range []string{"mkdir -p", "mv ", "rm -rf"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected an exec containing %q; got:\n%s", want, joined)
		}
	}
}

// Snapshots are on by default now; the opt-OUT is what has to keep working.
func TestPauseRefusedWhenExplicitlyDisabled(t *testing.T) {
	t.Setenv("FC_MEM_SNAPSHOTS", "0")
	be := newFake()
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	if err := inst.Snapshot(context.Background(), rt.SnapshotRef{Path: "/x"}); err == nil {
		t.Fatal("Snapshot must refuse with FC_MEM_SNAPSHOTS=0")
	}
	// Control: with the variable unset it goes through to the backend.
	t.Setenv("FC_MEM_SNAPSHOTS", "")
	if err := inst.Snapshot(context.Background(), rt.SnapshotRef{Path: "/x"}); err != nil {
		t.Fatalf("Snapshot with FC_MEM_SNAPSHOTS unset: %v, want it to work by default", err)
	}
}

var _ rt.Runtime = (*fcRuntime)(nil)

// The create env lives in the guest's /etc/template.env (SetEnv), never on an
// exec command line where /proc/*/cmdline would show it.
func TestBootEnvNotOnExecCommandLine(t *testing.T) {
	be := newFake()
	inst, err := newWithBackend(be).Boot(context.Background(), rt.BootSpec{
		SandboxID: "sb1",
		Template:  rt.Template{Name: "code-interpreter", Env: []string{"B7TOKEN=secret123"}},
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	_, _ = inst.Exec(context.Background(), rt.ExecSpec{Cmd: "printenv B7TOKEN", Env: map[string]string{"MODE": "exec"}})
	last := be.execs[len(be.execs)-1]
	if strings.Contains(last, "secret123") {
		t.Fatalf("create env rode the exec command line: %q", last)
	}
	if !strings.HasPrefix(last, "env ") || !strings.Contains(last, "MODE=exec") {
		t.Fatalf("per-exec env must still be injected: %q", last)
	}
}

func TestBootEnvNotOnStartServerCommandLine(t *testing.T) {
	be := newFake()
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{
		SandboxID: "sb1",
		Template:  rt.Template{Env: []string{"DATABASE_URL=postgres://x"}},
	})
	if err := inst.StartServer(context.Background(), "./app", map[string]string{"EXTRA": "1"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	last := be.detachedExecs[len(be.detachedExecs)-1]
	for _, want := range []string{"PORT=8000", "EXTRA=1"} {
		if !strings.Contains(last, want) {
			t.Fatalf("start command missing %q: %q", want, last)
		}
	}
	if strings.Contains(last, "postgres://x") {
		t.Fatalf("create env rode the start command line: %q", last)
	}
}

// Even a no-env exec carries $PORT, for parity with the start command; an explicit PORT wins.
func TestNoEnvExecCarriesAppPort(t *testing.T) {
	be := newFake()
	inst, _ := newWithBackend(be).Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	_, _ = inst.Exec(context.Background(), rt.ExecSpec{Cmd: "echo plain"})
	last := be.execs[len(be.execs)-1]
	if !strings.Contains(last, "PORT=8000") || !strings.Contains(last, "echo plain") {
		t.Fatalf("no-env exec must inject PORT=8000 around the command, got %q", last)
	}
	_, _ = inst.Exec(context.Background(), rt.ExecSpec{Cmd: "echo plain", Env: map[string]string{"PORT": "9999"}})
	last = be.execs[len(be.execs)-1]
	if !strings.Contains(last, "PORT=9999") || strings.Contains(last, "PORT=8000") {
		t.Fatalf("explicit PORT must win, got %q", last)
	}
}

func TestProcsParsesPsOutput(t *testing.T) {
	be := newFake()
	be.execOut["ps -eo"] = "    1 sleep\n  542 python3\n  600 my server\n"
	r := newWithBackend(be)
	inst, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	procs, err := inst.Procs()
	if err != nil {
		t.Fatalf("Procs err: %v", err)
	}
	if len(procs) != 3 {
		t.Fatalf("got %d procs, want 3: %+v", len(procs), procs)
	}
	if procs[0].PID != 1 || procs[0].Cmd != "sleep" {
		t.Errorf("proc[0] = %+v, want {1 sleep}", procs[0])
	}
	if procs[2].PID != 600 || procs[2].Cmd != "my server" {
		t.Errorf("proc[2] = %+v, want {600 my server}", procs[2])
	}
}

func TestProcsBestEffortOnFailure(t *testing.T) {
	be := newFake() // no execOut for ps, so empty stdout and an empty list
	r := newWithBackend(be)
	inst, _ := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "t"}})
	procs, err := inst.Procs()
	if err != nil {
		t.Fatalf("Procs should be best-effort, got err: %v", err)
	}
	if len(procs) != 0 {
		t.Fatalf("want empty, got %+v", procs)
	}
}

func TestSnapshotResumeWiringWithFlag(t *testing.T) {
	t.Setenv("FC_MEM_SNAPSHOTS", "1")
	be := newFake()
	r := newWithBackend(be)
	if !r.Capabilities().Snapshot {
		t.Fatal("Caps.Snapshot should be true with FC_MEM_SNAPSHOTS=1")
	}
	inst, _ := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "t"}})

	if err := inst.Snapshot(context.Background(), rt.SnapshotRef{Path: "/snap/sb1"}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(be.snapshotted) != 1 || be.snapshotted[0] != "vm_123→/snap/sb1" {
		t.Fatalf("SnapshotSandbox not called right: %v", be.snapshotted)
	}

	ri, err := r.Restore(context.Background(), rt.SnapshotRef{Path: "/snap/sb1"},
		rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "t"}})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if ri == nil || len(be.restored) != 1 {
		t.Fatalf("RestoreSandbox not called: %v", be.restored)
	}
	if tgt, _ := ri.ListenTarget(8000); tgt != "192.168.30.9:8000" {
		t.Fatalf("restored ListenTarget = %q", tgt)
	}
}

// Changed, not deleted: this pinned "off by default" and now pins the opt-out.
func TestSnapshotOnByDefaultAndDisablable(t *testing.T) {
	t.Setenv("FC_MEM_SNAPSHOTS", "")
	if !newWithBackend(newFake()).Capabilities().Snapshot {
		t.Fatal("Caps.Snapshot must default TRUE with FC_MEM_SNAPSHOTS unset")
	}
	t.Setenv("FC_MEM_SNAPSHOTS", "0")
	be := newFake()
	r := newWithBackend(be)
	if r.Capabilities().Snapshot {
		t.Fatal("Caps.Snapshot must be false with FC_MEM_SNAPSHOTS=0")
	}
	inst, _ := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	if err := inst.Snapshot(context.Background(), rt.SnapshotRef{Path: "/x"}); err == nil {
		t.Fatal("Snapshot should error when disabled")
	}
	if _, err := r.Restore(context.Background(), rt.SnapshotRef{Path: "/x"}, rt.BootSpec{SandboxID: "sb1"}); err == nil {
		t.Fatal("Restore should error when disabled")
	}
	if len(be.snapshotted) != 0 || len(be.restored) != 0 {
		t.Fatal("backend must not be called when disabled")
	}
}

// TestConfineShell runs the generated guard through the host's /bin/sh. Needs
// GNU realpath -m, so it skips off Linux.
func TestConfineShell(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires GNU coreutils realpath -m (guest contract); not available on this host's /bin/sh")
	}
	root := "/root"
	cases := []struct {
		p      string
		wantOK bool
	}{
		{"/root/ok.txt", true},
		{"ok.txt", false}, // relative, but the test CWD is not root
		{"/etc/passwd", false},
		{"/root/../etc/passwd", false},
	}
	for _, tc := range cases {
		t.Run(tc.p, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", confineShell(root, tc.p)+"true")
			err := cmd.Run()
			if (err == nil) != tc.wantOK {
				t.Fatalf("confineShell(%q) run err=%v wantOK=%v", tc.p, err, tc.wantOK)
			}
		})
	}
}

// TestConfineShellRelativeAgainstCWDRoot pins the CWD to root, as envd does
// for an empty cwd, so relative paths resolve the way they do in the guest.
func TestConfineShellRelativeAgainstCWDRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires GNU coreutils realpath -m (guest contract); not available on this host's /bin/sh")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		p      string
		wantOK bool
	}{
		{"ok.txt", true},
		{"sub/new.txt", true}, // realpath -m does not require the leaf to exist
		{"../etc/passwd", false},
	}
	for _, tc := range cases {
		t.Run(tc.p, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", confineShell(root, tc.p)+"true")
			cmd.Dir = root
			err := cmd.Run()
			if (err == nil) != tc.wantOK {
				t.Fatalf("confineShell(%q) run (cwd=root) err=%v wantOK=%v", tc.p, err, tc.wantOK)
			}
		})
	}
}

// The claim "this node enforces egress policy" is what the daemon trusts when
// it decides whether to refuse a create. On the shared bridge it is false: the
// host chains see only the post-SNAT source, so there is nowhere to hang a
// per-sandbox rule and a policy would be accepted and silently ignored.
func TestEgressPolicyCapabilityFollowsNetNSMode(t *testing.T) {
	r := newWithBackend(newFake())

	r.netns = false
	if r.Capabilities().EgressPolicy {
		t.Error("a bridge-mode node claims it enforces egress policy; every create carrying one " +
			"would be accepted and the sandbox would come up reading as confined and reaching everything")
	}

	r.netns = true
	if !r.Capabilities().EgressPolicy {
		t.Error("a netns node denies it enforces egress policy, so every policy-carrying create is " +
			"refused on the one runtime that can actually apply the rules")
	}
}
