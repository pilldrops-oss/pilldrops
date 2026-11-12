//go:build linux

package vm

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
)

// fcMachine wraps the Firecracker SDK Machine to implement FirecrackerMachine.
type fcMachine struct {
	m *firecracker.Machine
}

func (f *fcMachine) PauseVM(ctx context.Context) error  { return f.m.PauseVM(ctx) }
func (f *fcMachine) ResumeVM(ctx context.Context) error { return f.m.ResumeVM(ctx) }
func (f *fcMachine) Pid() int                           { pid, _ := f.m.PID(); return pid }

// Stop takes the VMM down in bounded time. The SDK's StopVMM only sends
// SIGTERM, so escalate to SIGKILL and confirm the process is gone before
// the caller tears down the chroot and overlay.
func (f *fcMachine) Stop(ctx context.Context) error {
	return stopVMMBounded(f.Pid(), f.m.StopVMM)
}

// stopVMMBounded escalates SIGTERM to SIGKILL and confirms the process is
// gone. sigterm is the SDK's StopVMM.
func stopVMMBounded(pid int, sigterm func() error) error {
	err := sigterm()
	if pid <= 0 {
		// No pid to escalate against.
		return err
	}
	if waitProcGone(pid, 2*time.Second) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if waitProcGone(pid, 3*time.Second) {
		return nil
	}
	// Unreaped or stuck in the kernel. Report rather than block; a later sweep reclaims.
	return fmt.Errorf("VMM pid %d still present after SIGTERM then SIGKILL", pid)
}

// waitProcGone polls kill(pid, 0) until ESRCH or the grace expires. A zombie still answers.
func waitProcGone(pid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// ChrootRoot returns the host path of this VM's jailer chroot root, built from
// the actual jailer id, which is "pool-<hex>" for a warm-pool VM rather than
// the sandbox id.
func (f *fcMachine) ChrootRoot() string {
	j := f.m.Cfg.JailerCfg
	if j == nil {
		return ""
	}
	return filepath.Join(j.ChrootBaseDir, filepath.Base(j.ExecFile), j.ID, "root")
}

func (f *fcMachine) CreateSnapshot(ctx context.Context, statePath, memPath string) error {
	// SDK order is (mem, state); swapping them produces a snapshot restore reads inverted.
	return f.m.CreateSnapshot(ctx, memPath, statePath)
}

func (f *fcMachine) CreateDiffSnapshot(ctx context.Context, statePath, memPath string) error {
	return f.m.CreateSnapshot(ctx, memPath, statePath, func(p *ops.CreateSnapshotParams) {
		p.Body.SnapshotType = models.SnapshotCreateParamsSnapshotTypeDiff
	})
}

// jailerBaseDir is where the jailer builds each VM's chroot. FC_JAILER_DIR
// overrides it: the SDK hard-links the overlay into the chroot, so it must be
// on the same filesystem as OVERLAY_DIR (a reflink-capable one for fork).
var jailerBaseDir = envOr("FC_JAILER_DIR", "/srv/jailer")

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func jailerChrootDir() string { return filepath.Join(jailerBaseDir, "firecracker") }

// cleanStaleJailer removes the chroot and cgroup left by a prior run of this
// sandbox id. The overlay and snapshot are hard-linked in, so the sources survive.
func cleanStaleJailer(id string) {
	cleanJailerRoot(filepath.Join(jailerChrootDir(), id, "root"))
}

// cleanJailerRoot removes the jailer dir and cgroup owning a ChrootRoot() path.
// Takes the path, not a sandbox id, because a pool VM's jailer id differs.
// Best-effort: a still-exiting VMM may leave a cgroup for the next sweep, so never block.
func cleanJailerRoot(chrootRoot string) {
	if chrootRoot == "" {
		return
	}
	dir := filepath.Dir(chrootRoot) // …/<execBase>/<jailerID>
	_ = os.RemoveAll(dir)
	// Same leaf prepareVMMCgroup created.
	rmdirCgroupTree(vmmCgroupPath(filepath.Base(dir)))
}

// rmdirCgroupTree removes a cgroup directory bottom-up. os.RemoveAll fails on
// the kernel's unremovable control files; rmdir works once the cgroup is empty.
func rmdirCgroupTree(path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			rmdirCgroupTree(filepath.Join(path, e.Name()))
		}
	}
	_ = os.Remove(path)
}

// buildKernelArgs builds the guest's /proc/cmdline. Only the token hash rides it,
// never the token, since the cmdline is world-readable in the guest; it goes after
// init= so the kernel passes it through untouched. An empty hash appends nothing.
func buildKernelArgs(vmIP net.IP, sandboxID, tokenHash string) string {
	// ipv6.disable=1 is a security control: every nftables rule is in the ip
	// family, so an IPv6 guest stack would be unfiltered. Disabled at boot, the
	// guest cannot turn it back on.
	//
	// The fifth ip= field is the HOSTNAME. Left empty, the kernel names the guest
	// after its own address, and on an FC_NETNS node every sandbox holds the same
	// fixed 192.168.30.2 — measured on a live node: `hostname` in an ordinary
	// cold-booted sandbox printed 192.168.30.2. The restore paths push SETHOST
	// over vsock because a golden's name is baked into its memory image; a cold
	// boot has no such image and needs nothing but this field.
	args := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 ip=%s::192.168.30.1:255.255.0.0:%s:eth0:off init=/sbin/overlay-init",
		vmIP.String(), sandboxID,
	)
	if tokenHash != "" {
		args += " envd.token_hash=" + tokenHash
	}
	return args
}

// startFirecracker boots a new Firecracker VM from scratch (cold boot).
func startFirecracker(ctx context.Context, fc FirecrackerConfig) (FirecrackerMachine, error) {
	// Chroot-relative; the jailer only provisions /run, so a /tmp socket fails to bind.
	socketPath := filepath.Join("/run", fc.SandboxID+".sock")
	_ = os.Remove(socketPath)

	// A cold boot is usually the fallback after a snapshot restore under the
	// same id failed, and that attempt left its jail behind: the jailer then
	// panics on a /dev/net/tun that already exists, so every create that had
	// to fall back (after an upgrade, while goldens rebuild) failed outright.
	cleanStaleJailer(fc.SandboxID)

	vmIP, rootDrive, drives, nics, err := fc.devices()
	if err != nil {
		return nil, err
	}

	kernelArgs := buildKernelArgs(vmIP, fc.SandboxID, fc.TokenHash)
	vsocks := []firecracker.VsockDevice{
		{
			// Chroot-relative. The host must discover this file rather than
			// rebuild the name: a restore keeps the snapshotted device's socket
			// name. See vsockUDSPath.
			Path: filepath.Join("/run", fc.SandboxID+".vsock"),
			CID:  3, // Guest CID
		},
	}

	// The SDK hard-links the kernel and rootfs into the chroot before the
	// jailer runs and the jailer leaves them alone, so make them readable
	// for the unprivileged uid here.
	if err := prepareSharedVMMInputs(&fc, fc.Config.KernelPath, rootDrive); err != nil {
		return nil, err
	}
	cfg := buildColdConfig(&fc, kernelArgs, socketPath, drives, nics, vsocks)

	// Written before the jailer runs so the guest boot happens inside the ceiling.
	vcpuCount, memSize, _ := resolveVCPUAndMem(&fc)
	if err := prepareVMMCgroup(fc.SandboxID, memSize, vcpuCount); err != nil {
		return nil, err
	}

	m, err := firecracker.NewMachine(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new machine: %w", err)
	}

	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("start machine: %w", err)
	}

	return &fcMachine{m: m}, nil
}

// rootDrive is the read-only image this VM boots: the one the caller named, or
// the one the template name resolves to on this node.
func (fc FirecrackerConfig) rootDrive() string {
	if fc.RootfsPath != "" {
		return fc.RootfsPath
	}
	return filepath.Join(fc.Config.SnapshotDir, fc.Template, "rootfs.squashfs")
}

// devices is the drive and NIC set both boot paths attach: the same read-only
// root, the same writable overlay and one tap. Described identically on both
// paths on purpose -- a restore has to present the devices its snapshot was
// taken with.
func (fc FirecrackerConfig) devices() (net.IP, string, []models.Drive, []firecracker.NetworkInterface, error) {
	vmIP, ok := fc.VMIP.(net.IP)
	if !ok {
		return nil, "", nil, nil, fmt.Errorf("invalid VMIP type")
	}
	rootDrive := fc.rootDrive()
	drives := []models.Drive{
		{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(rootDrive),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(true),
		},
		{
			DriveID:      firecracker.String("overlay"),
			PathOnHost:   firecracker.String(fc.OverlayPath),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(false),
		},
	}
	for i, v := range fc.Volumes {
		drives = append(drives, models.Drive{
			DriveID:      firecracker.String(fmt.Sprintf("vol%d", i)),
			PathOnHost:   firecracker.String(v),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(false),
		})
	}
	nics := []firecracker.NetworkInterface{
		{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  generateMAC(vmIP),
				HostDevName: fc.TapName,
			},
		},
	}
	return vmIP, rootDrive, drives, nics, nil
}

// restoreFirecrackerSnapshot restores a VM from a snapshot.
func restoreFirecrackerSnapshot(fc FirecrackerConfig) (FirecrackerMachine, error) {
	// Chroot-relative; the jailer only provisions /run.
	socketPath := filepath.Join("/run", fc.SandboxID+".sock")
	_ = os.Remove(socketPath)

	// A resume reuses the sandbox id and the jailer refuses an existing
	// chroot/cgroup, so clear the previous run's first.
	cleanStaleJailer(fc.SandboxID)

	_, rootDrive, drives, nics, err := fc.devices()
	if err != nil {
		return nil, err
	}

	// The mem/state pair needs no chmod: a pause/resume snapshot is owned by
	// this uid, and a golden one is made readable at creation.
	if err := prepareSharedVMMInputs(&fc, fc.Config.KernelPath, rootDrive); err != nil {
		return nil, err
	}
	cfg := buildRestoreConfig(&fc, socketPath, drives, nics)

	// Must follow cleanStaleJailer, which rmdir's this sandbox's previous leaf.
	vcpuCount, memSize, _ := resolveVCPUAndMem(&fc)
	if err := prepareVMMCgroup(fc.SandboxID, memSize, vcpuCount); err != nil {
		return nil, err
	}

	ctx := context.Background()

	// cfg.Snapshot alone never loads the snapshot; only WithSnapshot wires the
	// load handler. WithSnapshot's fresh handler list drops the jailer's
	// chroot-linking, so relink re-applies it.
	snapFiles := []string{fc.MemPath, fc.StatePath}
	if fc.MemSource != nil {
		snapFiles = []string{fc.StatePath} // no memory file: it is served lazily
	}
	relink := func(mm *firecracker.Machine) {
		_ = newSnapshotChrootStrategy(fc.Config.KernelPath, snapFiles...).AdaptHandlers(&mm.Handlers)
		if fc.LazyMem || fc.MemSource != nil {
			mm.Handlers.FcInit = mm.Handlers.FcInit.Swap(uffdLoadHandler(fc.MemPath, fc.MemSource))
		}
		if fc.MemSource != nil {
			// It stats the memory file, which a lazy restore does not have.
			mm.Handlers.Validation = mm.Handlers.Validation.Remove(firecracker.ValidateLoadSnapshotCfgHandlerName)
		}
	}
	m, err := firecracker.NewMachine(ctx, cfg,
		firecracker.WithSnapshot(fc.MemPath, fc.StatePath),
		relink,
	)
	if err != nil {
		return nil, fmt.Errorf("new machine for snapshot restore: %w", err)
	}

	if err := m.Start(ctx); err != nil {
		// A memory image carries its host's TSC frequency, and a host without
		// hardware TSC scaling cannot restore one from a host whose frequency
		// differs (measured: 3292 vs 3392 MHz, one direction). That is this
		// host's CPU refusing the capture, so it reads as ErrCPUTooPoor and
		// the control plane tries another node, as for missing instructions.
		if strings.Contains(err.Error(), "TSC scaling") {
			return nil, fmt.Errorf("start from snapshot: %w: this host cannot match the TSC frequency "+
				"the capture was taken at: %v", ErrCPUTooPoor, err)
		}
		return nil, fmt.Errorf("start from snapshot: %w", err)
	}

	return &fcMachine{m: m}, nil
}

// resolveVCPUAndMem applies the request's VCPUs/MemoryMB, then the template's
// default, which is also what a golden restore's guest was built with.
func resolveVCPUAndMem(fc *FirecrackerConfig) (vcpuCount, memSize int64, smt bool) {
	defVCPU, defMem := defaultResources(fc.Config, fc.Template)
	vcpuCount, memSize = int64(fc.VCPUs), int64(fc.MemoryMB)
	if vcpuCount <= 0 {
		vcpuCount = int64(defVCPU)
	}
	if memSize <= 0 {
		memSize = int64(defMem)
	}
	return vcpuCount, memSize, false
}

// noForwardedSignals turns off the SDK's signal forwarding. Left nil, the SDK
// catches SIGTERM, SIGINT, SIGHUP, SIGQUIT and SIGABRT sent to this daemon and
// forwards each to every Firecracker it started, so a `systemctl stop` or an
// upgrade killed every guest before the daemon's own shutdown could pause it,
// and the restart then found nothing to resume: every running sandbox lost.
// Non-nil and empty is the SDK's "forward nothing".
var noForwardedSignals = []os.Signal{}

// buildColdConfig assembles the cold-boot firecracker.Config; pure, so it is
// unit-testable anywhere. The jailer uid is derived via vmUID so the default is
// observable from a test. Seccomp is always on; an empty Filter means Firecracker's default.
func buildColdConfig(fc *FirecrackerConfig, kernelArgs string, socketPath string, drives []models.Drive, nics []firecracker.NetworkInterface, vsocks []firecracker.VsockDevice) firecracker.Config {
	uid := vmUID(fc.Config.NonRootVMM, fc.SandboxID)
	vcpuCount, memSize, smt := resolveVCPUAndMem(fc)
	return firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: fc.Config.KernelPath,
		KernelArgs:      kernelArgs,
		// The jailer enters this netns before exec'ing firecracker; "" means
		// the root netns (bridge path).
		NetNS: fc.NetNS,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:       &vcpuCount,
			MemSizeMib:      &memSize,
			Smt:             &smt,
			TrackDirtyPages: true, // see EnableDiffSnapshots in buildRestoreConfig
		},
		Drives:            drives,
		NetworkInterfaces: nics,
		VsockDevices:      vsocks,
		Seccomp:           firecracker.SeccompConfig{Enabled: true},
		ForwardSignals:    noForwardedSignals,
		JailerCfg: &firecracker.JailerConfig{
			GID:           firecracker.Int(uid),
			UID:           firecracker.Int(uid),
			NumaNode:      firecracker.Int(0),
			ExecFile:      fc.Config.FirecrackerBin,
			JailerBinary:  fc.Config.JailerBin,
			ChrootBaseDir: jailerBaseDir,
			ID:            fc.SandboxID,
			// Required: the SDK's jail() dereferences ChrootStrategy.
			ChrootStrategy: firecracker.NewNaiveChrootStrategy(fc.Config.KernelPath),
			// v2-only hosts fail with CgroupHierarchyMissing on the jailer's v1 default.
			CgroupVersion: "2",
		},
	}
}

// buildRestoreConfig assembles the firecracker.Config for a snapshot restore.
// No KernelArgs and no VsockDevices: both come back from the snapshot, and
// Firecracker rejects adding a vsock after resume.
func buildRestoreConfig(fc *FirecrackerConfig, socketPath string, drives []models.Drive, nics []firecracker.NetworkInterface) firecracker.Config {
	uid := vmUID(fc.Config.NonRootVMM, fc.SandboxID)
	vcpuCount, memSize, smt := resolveVCPUAndMem(fc)
	return firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: fc.Config.KernelPath,
		// See buildColdConfig.
		NetNS: fc.NetNS,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &vcpuCount,
			MemSizeMib: &memSize,
			Smt:        &smt,
		},
		Drives:            drives,
		NetworkInterfaces: nics,
		Seccomp:           firecracker.SeccompConfig{Enabled: true},
		ForwardSignals:    noForwardedSignals,
		Snapshot: firecracker.SnapshotConfig{
			MemFilePath:  fc.MemPath,
			SnapshotPath: fc.StatePath,
			ResumeVM:     true,
			// Dirty-page tracking from the first instruction, so a later fork or
			// pause writes the touched pages rather than faulting in every page
			// of a file-backed guest (2.1 s for 512 MiB measured; see memsnap.go).
			EnableDiffSnapshots: true,
		},
		JailerCfg: &firecracker.JailerConfig{
			GID:            firecracker.Int(uid),
			UID:            firecracker.Int(uid),
			NumaNode:       firecracker.Int(0),
			ExecFile:       fc.Config.FirecrackerBin,
			JailerBinary:   fc.Config.JailerBin,
			ChrootBaseDir:  jailerBaseDir,
			ID:             fc.SandboxID,
			ChrootStrategy: newSnapshotChrootStrategy(fc.Config.KernelPath, fc.MemPath, fc.StatePath),
			// v2-only hosts fail with CgroupHierarchyMissing on the jailer's v1 default.
			CgroupVersion: "2",
		},
	}
}

// snapshotChrootStrategy is NaiveChrootStrategy plus mirroring the snapshot
// mem/state files into the chroot at their absolute host path, so the jailed
// VMM and the host-side path validation both find them.
type snapshotChrootStrategy struct {
	kernelImagePath string
	snapshotFiles   []string // absolute host paths to mirror into the chroot
}

func newSnapshotChrootStrategy(kernelImagePath string, snapshotFiles ...string) snapshotChrootStrategy {
	return snapshotChrootStrategy{kernelImagePath: kernelImagePath, snapshotFiles: snapshotFiles}
}

func (s snapshotChrootStrategy) AdaptHandlers(handlers *firecracker.Handlers) error {
	if !handlers.FcInit.Has(firecracker.CreateLogFilesHandlerName) {
		return firecracker.ErrRequiredHandlerMissing
	}
	// Same kernel/drive linking as NaiveChrootStrategy.
	handlers.FcInit = handlers.FcInit.AppendAfter(
		firecracker.CreateLogFilesHandlerName,
		firecracker.LinkFilesHandler(filepath.Base(s.kernelImagePath)),
	)
	// Mirror the snapshot files before LoadSnapshot runs.
	handlers.FcInit = handlers.FcInit.AppendAfter(
		firecracker.LinkFilesToRootFSHandlerName,
		firecracker.Handler{
			Name: "fcinit.LinkSnapshotFiles",
			Fn: func(_ context.Context, m *firecracker.Machine) error {
				rootfs := filepath.Join(
					m.Cfg.JailerCfg.ChrootBaseDir,
					filepath.Base(m.Cfg.JailerCfg.ExecFile),
					m.Cfg.JailerCfg.ID,
					"root",
				)
				for _, host := range s.snapshotFiles {
					dst := filepath.Join(rootfs, host)
					if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
						return fmt.Errorf("mkdir snapshot chroot path: %w", err)
					}
					_ = os.Remove(dst) // clear a stale link from a prior boot
					if err := linkOrCopy(host, dst); err != nil {
						return fmt.Errorf("mirror snapshot file %s into chroot: %w", host, err)
					}
				}
				return nil
			},
		},
	)
	return nil
}

// copyFile copies src to dst, creating or truncating dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// linkOrCopy hard-links src to dst, copying when they are on different filesystems.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// generateMAC derives AA:FC:<ip4> from the VM's IP; a sandbox-id prefix
// collides across pool-* ids.
func generateMAC(vmIP net.IP) string {
	ip4 := vmIP.To4()
	if ip4 == nil {
		ip4 = []byte{0, 0, 0, 0}
	}
	return fmt.Sprintf("AA:FC:%02x:%02x:%02x:%02x", ip4[0], ip4[1], ip4[2], ip4[3])
}

// uidForSandbox hashes the full id into a stable unprivileged uid
// (10000-60000); deterministic so pause/resume reuses it.
func uidForSandbox(sandboxID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sandboxID))
	return 10000 + int(h.Sum32()%50000)
}

// vmUID is the single reader of the NonRootVMM gate: the uid the jailer runs
// the VMM as and the owner of its overlay and tap. FC_NONROOT_VMM=0 puts the
// VMM back on uid 0; an escape hatch, not a supported configuration.
func vmUID(nonRoot bool, sandboxID string) int {
	if nonRoot {
		return uidForSandbox(sandboxID)
	}
	return 0
}

// prepareSharedVMMInputs makes the kernel and template rootfs readable by an
// unprivileged VMM. No-op as root. Fails the boot rather than falling back to root.
func prepareSharedVMMInputs(fc *FirecrackerConfig, paths ...string) error {
	if vmUID(fc.Config.NonRootVMM, fc.SandboxID) == 0 {
		return nil
	}
	for _, p := range paths {
		if err := ensureVMMReadable(p); err != nil {
			return fmt.Errorf("make %s readable by the unprivileged VMM: %w", p, err)
		}
	}
	return nil
}

// ensureVMMReadable adds group+other read. A chmod, never a chown: the chroot
// entry is a hard link, so chowning would hand one sandbox ownership of the
// kernel every other sandbox boots from. Template-scoped inputs only.
func ensureVMMReadable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := fi.Mode().Perm()
	if mode&0o044 == 0o044 {
		return nil
	}
	return os.Chmod(path, mode|0o044)
}

// scanFirecrackerProcs is the one /proc read behind findFirecrackerProcesses
// and liveJailerIDs. ok is false only when reading /proc itself failed, never
// because nothing matched.
func scanFirecrackerProcs() (procs []*OrphanProcess, ok bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}

		cmd := string(cmdline)
		if !strings.Contains(cmd, "firecracker") || !strings.Contains(cmd, "--id") {
			continue
		}

		sandboxID := extractSandboxID(cmd)
		if sandboxID == "" {
			continue
		}

		procs = append(procs, &OrphanProcess{
			SandboxID: sandboxID,
			Machine:   &procKiller{pid: pid},
		})
	}
	return procs, true
}

// findFirecrackerProcesses scans /proc for Firecracker processes; a failed
// scan reads as nothing to recover.
func findFirecrackerProcesses() []*OrphanProcess {
	procs, _ := scanFirecrackerProcs()
	return procs
}

// liveJailerIDs returns the jailer --ids with a live process. ok is false only on
// a /proc read failure; callers must treat that as unknown, not empty, since only
// this set protects a pool VM's chroot from the sweep. A var so tests can force ok=false.
var liveJailerIDs = func() (live map[string]bool, ok bool) {
	procs, ok := scanFirecrackerProcs()
	if !ok {
		return nil, false
	}
	live = make(map[string]bool, len(procs))
	for _, p := range procs {
		live[p.SandboxID] = true // field name is legacy; holds the jailer --id
	}
	return live, true
}

func extractSandboxID(cmdline string) string {
	parts := strings.Split(cmdline, "\x00")
	for i, p := range parts {
		if p == "--id" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// procKiller implements FirecrackerMachine for orphan cleanup.
type procKiller struct{ pid int }

func (p *procKiller) PauseVM(context.Context) error                            { return nil }
func (p *procKiller) ResumeVM(context.Context) error                           { return nil }
func (p *procKiller) CreateSnapshot(context.Context, string, string) error     { return nil }
func (p *procKiller) CreateDiffSnapshot(context.Context, string, string) error { return nil }
func (p *procKiller) Pid() int                                                 { return p.pid }
func (p *procKiller) ChrootRoot() string                                       { return "" }

// Stop kills the VMM. kill(-pid) only works when the process leads its own
// group, which an orphan reparented to init does not, so fall back to the pid.
func (p *procKiller) Stop(context.Context) error {
	if pgid, err := syscall.Getpgid(p.pid); err == nil && pgid == p.pid {
		if err := syscall.Kill(-p.pid, syscall.SIGKILL); err == nil {
			return nil
		}
	}
	return syscall.Kill(p.pid, syscall.SIGKILL)
}
