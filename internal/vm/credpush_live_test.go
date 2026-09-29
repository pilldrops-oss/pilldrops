//go:build linux

package vm

// Live verification: a real Firecracker VM, snapshotted and restored, given a
// credential it never booted with. Needs KVM, the jailer, a built template and
// root; skips elsewhere. Run on the reference host:
//
//	go test -c -o vm.test ./internal/vm/          (GOOS=linux)
//	scp vm.test $BENCH_HOST:/tmp/
//	sudo VM_LIVE_FC=1 VM_LIVE_ENVD=/tmp/envd-s2 \
//	     KERNEL_PATH=... FIRECRACKER_BIN=... JAILER_BIN=... \
//	     SNAPSHOT_DIR=... OVERLAY_DIR=... /tmp/vm.test -test.run Live -test.v
//
// VM_LIVE_ENVD is required because the template's baked envd may predate the
// credential channel; the binary is injected into the overlay's upper layer.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
)

func liveCfg(t *testing.T) (Config, string) {
	t.Helper()
	if os.Getenv("VM_LIVE_FC") != "1" {
		t.Skip("SKIP (not a pass): set VM_LIVE_FC=1 on a KVM host with a built template to run this")
	}
	if os.Geteuid() != 0 {
		t.Skip("SKIP (not a pass): the jailer and tap setup need root")
	}
	if os.Getenv("VM_LIVE_ENVD") == "" {
		t.Skip("SKIP (not a pass): set VM_LIVE_ENVD to a §1-built envd binary. Without it " +
			"the guest runs the template's baked envd, which may have no credential channel — " +
			"and the failure would look like a broken push rather than an old binary")
	}
	cfg := Config{
		SnapshotDir:    env(t, "SNAPSHOT_DIR"),
		OverlayDir:     env(t, "OVERLAY_DIR"),
		FirecrackerBin: env(t, "FIRECRACKER_BIN"),
		JailerBin:      env(t, "JAILER_BIN"),
		KernelPath:     env(t, "KERNEL_PATH"),
		MaxConcurrent:  4,
		MaxActive:      16,
		MemoryMB:       1024,
		VCPUs:          1,
		// Matches how the deployed VMM runs (uid 0).
		NonRootVMM: false,
	}
	tmpl := os.Getenv("VM_LIVE_TEMPLATE")
	if tmpl == "" {
		tmpl = "base"
	}
	return cfg, tmpl
}

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("SKIP (not a pass): %s is unset", k)
	}
	return v
}

// withOverlayMounted loop-mounts a sandbox's writable image on the host;
// <mnt>/upper/x is /x inside the guest.
func withOverlayMounted(t *testing.T, overlayPath string, fn func(mnt string)) {
	t.Helper()
	mnt := t.TempDir()
	if out, err := exec.Command("mount", "-o", "loop", overlayPath, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount overlay %s: %v: %s", overlayPath, err, out)
	}
	defer func() {
		// sync so the byte-for-byte copy that follows sees a flushed image.
		_ = exec.Command("sync").Run()
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			t.Errorf("umount overlay: %v: %s", err, out)
		}
	}()
	fn(mnt)
}

// injectEnvd shadows the template's baked envd via the overlay's upper layer.
func injectEnvd(t *testing.T, overlayPath, envdBin string) {
	t.Helper()
	b, err := os.ReadFile(envdBin)
	if err != nil {
		t.Fatalf("read %s: %v", envdBin, err)
	}
	withOverlayMounted(t, overlayPath, func(mnt string) {
		dst := filepath.Join(mnt, "upper", "usr", "local", "bin")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatalf("mkdir in overlay: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dst, "envd"), b, 0o755); err != nil {
			t.Fatalf("write envd into overlay: %v", err)
		}
	})
}

// execIn runs one command in the guest and returns stdout.
func execIn(t *testing.T, c *envd.Client, cmd string) string {
	t.Helper()
	res, err := c.Execute(context.Background(), cmd, "/root", 15*time.Second)
	if err != nil {
		t.Fatalf("exec %q: %v", cmd, err)
	}
	return strings.TrimSpace(string(res.Stdout))
}

// liveManager must run before any VM exists: NewManager's RecoverOrphans kills
// every Firecracker process not in its active set.
func liveManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m, err := NewManager(cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// bootGoldenVM cold-boots a VM with no token hash on its cmdline, the
// golden-template condition. prep runs against the overlay before boot.
func bootGoldenVM(t *testing.T, cfg Config, tmpl, id string, prep ...goldenPrep) (FirecrackerMachine, *NetworkSetup, string) {
	t.Helper()
	uid := vmUID(cfg.NonRootVMM, id)

	netw, err := SetupVMNetwork(id, uid)
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	overlay, err := createOverlay(id, uid, DefaultOverlayMB, cfg)
	if err != nil {
		TeardownVMNetwork(netw.TapName)
		t.Fatalf("overlay: %v", err)
	}
	injectEnvd(t, overlay, os.Getenv("VM_LIVE_ENVD"))
	for _, p := range prep {
		p(t, overlay)
	}

	machine, err := startFirecracker(context.Background(), FirecrackerConfig{
		SandboxID: id, Template: tmpl,
		TapName: netw.TapName, VMIP: netw.VMIP,
		OverlayPath: overlay,
		VCPUs:       1, MemoryMB: 1024,
		TokenHash: "", // no envd.token_hash= on the cmdline
		Config:    cfg,
	})
	if err != nil {
		deleteOverlay(overlay)
		TeardownVMNetwork(netw.TapName)
		t.Fatalf("start firecracker: %v", err)
	}
	return machine, netw, overlay
}

// Every cold create goes through the authenticated readiness probe, so this
// runs the full production path against the template's own baked envd.
func TestLiveColdCreateStillWorks(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	cfg.EnvdAuth = true // the production default; makes Create mint a token
	cfg.Templates = []string{tmpl}
	ctx := context.Background()
	m := liveManager(t, cfg)

	sb, err := m.Create(ctx, CreateRequest{Template: tmpl, UserID: "live-test"})
	if err != nil {
		t.Fatalf("cold create: %v — authenticated readiness has broken the path "+
			"every sandbox on this node uses", err)
	}
	defer func() { _ = m.Destroy(ctx, sb.ID) }()

	if sb.BootType != "cold" {
		t.Fatalf("boot type = %q, want cold", sb.BootType)
	}
	if sb.Token == "" {
		t.Fatal("EnvdAuth is on but Create minted no token; the readiness probe " +
			"below would then be proving nothing")
	}
	res, err := m.Exec(ctx, sb.ID, "echo cold-ok && grep -c envd.token_hash= /proc/cmdline", "", 15*time.Second)
	if err != nil {
		t.Fatalf("exec on a cold-created sandbox: %v", err)
	}
	out := strings.TrimSpace(string(res.Stdout))
	if !strings.Contains(out, "cold-ok") {
		t.Fatalf("exec output %q", out)
	}
	// A cold guest carries the hash on its cmdline; a restored one must not.
	if !strings.Contains(out, "1") {
		t.Errorf("a cold-booted guest's /proc/cmdline has no envd.token_hash= (%q); "+
			"the restored guests' lack of one would then prove nothing", out)
	}
	t.Logf("cold create %s (%s, %d ms) -> authenticated exec ok: %q",
		sb.ID, sb.BootType, sb.BootMs, out)
}

// One restored VM, one token: refused before the push, allowed after. The
// refusal comes first so a readiness check that ignores the credential cannot pass.
func TestLiveCredentialControlPair(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	ctx := context.Background()
	m := liveManager(t, cfg) // before any VM exists

	goldenID := "s2golden-" + randomHex(4)
	machine, netw, overlay := bootGoldenVM(t, cfg, tmpl, goldenID)
	addr := netw.VMIP.String() + ":6001"
	t.Logf("golden VM %s at %s, chroot %s", goldenID, addr, machine.ChrootRoot())

	cleanedUp := false
	defer func() {
		if !cleanedUp {
			_ = machine.Stop(ctx)
			deleteOverlay(overlay)
			TeardownVMNetwork(netw.TapName)
		}
	}()

	// Ready with no credential is the precondition for a golden snapshot.
	gc, err := envd.Connect(addr, 90*time.Second)
	if err != nil {
		t.Fatalf("golden VM never became Health-ready: %v — with no credential this is "+
			"either the injected envd failing to start or envdListenAddr refusing to bind", err)
	}
	gc.Close()
	t.Log("golden VM reached envd-ready with NO credential")

	// Ready but open would make everything below meaningless.
	preToken, _ := sbtoken.Mint()
	if c, err := envd.ConnectWithToken(addr, preToken, 5*time.Second); err == nil {
		c.Close()
		t.Fatal("the golden VM accepted an arbitrary token before any credential existed")
	}
	t.Log("golden VM refuses authenticated calls while uncredentialed")

	sbClient, err := envd.Connect(addr, 30*time.Second)
	if err != nil {
		t.Fatalf("reconnect before snapshot: %v", err)
	}
	m.mu.Lock()
	m.active[goldenID] = &Sandbox{
		ID: goldenID, Template: tmpl, Machine: machine, Envd: sbClient,
		OverlayPath: overlay, TapName: netw.TapName, VMIP: netw.VMIP.String(),
		Token: "", stopTimer: func() {},
	}
	m.mu.Unlock()

	snapDir := t.TempDir()
	if err := m.SnapshotSandbox(ctx, goldenID, snapDir); err != nil {
		t.Fatalf("SnapshotSandbox: %v", err)
	}
	cleanedUp = true // SnapshotSandbox stopped the VM and kept the overlay + tap
	defer func() {
		deleteOverlay(overlay)
		TeardownVMNetwork(netw.TapName)
	}()

	// Writing a token the guest never saw is the whole golden-snapshot simulation.
	token, hash := sbtoken.Mint()
	meta, err := readSnapMeta(snapDir)
	if err != nil {
		t.Fatalf("readSnapMeta: %v", err)
	}
	if meta.Token != "" {
		t.Fatalf("meta.Token = %q, want empty — the golden VM was booted without one", meta.Token)
	}
	meta.Token = token
	if err := writeSnapMeta(snapDir, meta); err != nil {
		t.Fatalf("writeSnapMeta: %v", err)
	}

	// Restore under a different id, as adapter.go's Restore does, so the run
	// measures whether the socket file name diverges from the chroot name.
	restoreID := "s2restore-" + randomHex(4)
	netw2 := &NetworkSetup{TapName: meta.TapName, VMIP: netw.VMIP}
	if meta.TapName == "" || !tapExists(meta.TapName) {
		n, err := SetupVMNetworkWithIP(netw.VMIP, vmUID(cfg.NonRootVMM, restoreID))
		if err != nil {
			t.Fatalf("network (same IP): %v", err)
		}
		netw2 = n
	}
	machine2, err := restoreFirecrackerSnapshot(FirecrackerConfig{
		SandboxID: restoreID, Template: tmpl,
		TapName: netw2.TapName, VMIP: netw2.VMIP,
		OverlayPath: meta.OverlayPath,
		StatePath:   filepath.Join(snapDir, "state"),
		MemPath:     filepath.Join(snapDir, "mem"),
		VCPUs:       1, MemoryMB: 1024,
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	defer func() { _ = machine2.Stop(ctx) }()

	chroot := machine2.ChrootRoot()
	uds, err := vsockUDSPath(chroot, restoreID)
	if err != nil {
		t.Fatalf("vsockUDSPath(%s, %s): %v", chroot, restoreID, err)
	}
	t.Logf("MEASURED restore id=%s\n  chroot: %s\n  uds:    %s", restoreID, chroot, uds)

	// Otherwise everything below fails for an unrelated reason.
	if _, err := os.Stat(uds); err != nil {
		t.Fatalf("vsockUDSPath returned %s, which does not exist: %v", uds, err)
	}
	// Log whether discovery was load-bearing; not asserted, since restore naming may change.
	naive := filepath.Join(chroot, "run", restoreID+".vsock")
	if _, err := os.Stat(naive); err != nil {
		t.Logf("MEASURED: the socket file name (%s) does NOT match the restore id — "+
			"the path a caller would have reconstructed (%s) does not exist, so discovery "+
			"was load-bearing on this run", filepath.Base(uds), naive)
	} else {
		t.Logf("MEASURED: the reconstructed path %s happens to exist on this run; discovery "+
			"was not load-bearing here, and the divergence has to be re-measured rather than "+
			"assumed", naive)
	}

	t.Log("--- CONTROL A: restored, credential NOT yet pushed -> must be REFUSED")
	if c, err := envd.ConnectWithToken(addr, token, 8*time.Second); err == nil {
		c.Close()
		t.Fatal("the restored VM accepted this sandbox's token BEFORE the credential was pushed. " +
			"Either readiness is not authenticated, or the guest kept a hash it should not have — " +
			"and without this half, the probe below could not fail")
	} else {
		t.Logf("CONTROL A refused, as required: %v", err)
	}

	t.Log("--- push this sandbox's credential over ITS OWN vsock UDS")
	if err := deliverCredential(chroot, restoreID, hash, credPushDeadline); err != nil {
		t.Fatalf("deliverCredential: %v", err)
	}
	t.Log("SETHASH accepted by envd")

	t.Log("--- PROBE: same VM, same token -> must be ALLOWED")
	rc, err := envd.ConnectWithToken(addr, token, 30*time.Second)
	if err != nil {
		t.Fatalf("the credential was accepted over vsock but the token still does not work: %v", err)
	}
	defer rc.Close()
	if got := execIn(t, rc, "echo restored-ok"); got != "restored-ok" {
		t.Fatalf("exec after the push returned %q", got)
	}
	t.Log("PROBE allowed: authenticated exec succeeded on the restored VM")

	// The credential must not have come from the boot arguments.
	cmdline := execIn(t, rc, "cat /proc/cmdline")
	t.Logf("/proc/cmdline (restored guest): %s", cmdline)
	if strings.Contains(cmdline, "envd.token_hash=") {
		t.Fatalf("the restored guest's cmdline carries envd.token_hash=; the credential may have "+
			"come from the boot arguments and the push proved nothing: %s", cmdline)
	}
	// Positive control: an unreadable cmdline would also contain no hash.
	if !strings.Contains(cmdline, "root=") {
		t.Fatalf("/proc/cmdline does not look like a kernel cmdline (%q); the absence of "+
			"envd.token_hash= above would prove nothing", cmdline)
	}

	// Control B: the credential is this sandbox's, not a skeleton key.
	wrong, _ := sbtoken.Mint()
	if c, err := envd.ConnectWithToken(addr, wrong, 5*time.Second); err == nil {
		c.Close()
		t.Fatal("a different, valid token was accepted after the push — the guest is not " +
			"enforcing THIS sandbox's credential")
	}
	t.Log("CONTROL B: a different sandbox's valid token is still refused")
}

// Drives the production RestoreSandbox so the push-after-resume ordering is
// proven there, not only by hand.
func TestLiveRestoreSandboxWiring(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	ctx := context.Background()
	m := liveManager(t, cfg)

	goldenID := "s2wire-" + randomHex(4)
	machine, netw, overlay := bootGoldenVM(t, cfg, tmpl, goldenID)
	addr := netw.VMIP.String() + ":6001"

	c, err := envd.Connect(addr, 90*time.Second)
	if err != nil {
		_ = machine.Stop(ctx)
		deleteOverlay(overlay)
		TeardownVMNetwork(netw.TapName)
		t.Fatalf("golden VM never became ready: %v", err)
	}

	m.mu.Lock()
	m.active[goldenID] = &Sandbox{
		ID: goldenID, Template: tmpl, Machine: machine, Envd: c,
		OverlayPath: overlay, TapName: netw.TapName, VMIP: netw.VMIP.String(),
		Token: "", stopTimer: func() {},
	}
	m.mu.Unlock()

	snapDir := t.TempDir()
	if err := m.SnapshotSandbox(ctx, goldenID, snapDir); err != nil {
		t.Fatalf("SnapshotSandbox: %v", err)
	}
	defer func() {
		deleteOverlay(overlay)
		TeardownVMNetwork(netw.TapName)
	}()

	token, _ := sbtoken.Mint()
	meta, err := readSnapMeta(snapDir)
	if err != nil {
		t.Fatalf("readSnapMeta: %v", err)
	}
	meta.Token = token
	if err := writeSnapMeta(snapDir, meta); err != nil {
		t.Fatalf("writeSnapMeta: %v", err)
	}

	restoreID := "s2wired-" + randomHex(4)
	sb, err := m.RestoreSandbox(ctx, restoreID, snapDir, 1, 1024)
	if err != nil {
		t.Fatalf("RestoreSandbox: %v — the production path could not deliver a credential "+
			"the guest never booted with", err)
	}
	defer func() { _ = m.Destroy(ctx, restoreID) }()

	res, err := sb.Envd.Execute(ctx, "id -u && cat /proc/cmdline", "/root", 15*time.Second)
	if err != nil {
		t.Fatalf("exec on the restored sandbox: %v", err)
	}
	out := strings.TrimSpace(string(res.Stdout))
	t.Logf("RestoreSandbox -> authenticated exec ok:\n%s", out)
	if strings.Contains(out, "envd.token_hash=") {
		t.Fatalf("restored guest's cmdline carries a token hash: %s", out)
	}
}

// A snapshot whose meta carries no token must fail RestoreSandbox with a named
// error, not yield a sandbox that answers Unauthenticated to everything.
func TestLiveRestoreSandboxFailsLoudlyWithoutTheCredential(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	ctx := context.Background()
	m := liveManager(t, cfg)

	goldenID := "s2noc-" + randomHex(4)
	machine, netw, overlay := bootGoldenVM(t, cfg, tmpl, goldenID)
	addr := netw.VMIP.String() + ":6001"

	c, err := envd.Connect(addr, 90*time.Second)
	if err != nil {
		_ = machine.Stop(ctx)
		deleteOverlay(overlay)
		TeardownVMNetwork(netw.TapName)
		t.Fatalf("golden VM never became ready: %v", err)
	}

	m.mu.Lock()
	m.active[goldenID] = &Sandbox{
		ID: goldenID, Template: tmpl, Machine: machine, Envd: c,
		OverlayPath: overlay, TapName: netw.TapName, VMIP: netw.VMIP.String(),
		Token: "", stopTimer: func() {},
	}
	m.mu.Unlock()

	snapDir := t.TempDir()
	if err := m.SnapshotSandbox(ctx, goldenID, snapDir); err != nil {
		t.Fatalf("SnapshotSandbox: %v", err)
	}
	defer func() {
		deleteOverlay(overlay)
		TeardownVMNetwork(netw.TapName)
	}()

	// meta.Token stays "": no credential to deliver.
	sb, err := m.RestoreSandbox(ctx, "s2noc-r-"+randomHex(4), snapDir, 1, 1024)
	if err == nil {
		_ = m.Destroy(ctx, sb.ID)
		t.Fatal("RestoreSandbox reported a sandbox READY with no credential. " +
			"Every exec against it would answer Unauthenticated, and the node would bill " +
			"for it — which is exactly what Health-only readiness allowed")
	}
	if !strings.Contains(err.Error(), ErrCredentialPush.Error()) {
		t.Fatalf("err = %v; want it to name the credential push", err)
	}
	t.Logf("refused, named: %v", err)
}
