//go:build linux

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	"github.com/pilldrops-oss/pilldrops/internal/hoststats"
)

// sandboxSnapMeta makes a resume self-contained from the snapshot directory, so
// it survives a daemon restart.
type sandboxSnapMeta struct {
	// ID is the key the sandbox was registered under when it was snapshotted;
	// its lineage dir and token/endpoint/tap records live under that key.
	ID          string `json:"id,omitempty"`
	OverlayPath string `json:"overlay_path"`
	VMIP        string `json:"vmip"`
	TapName     string `json:"tap_name"` // kept alive across pause so resume reuses the same device
	Template    string `json:"template"`
	// RootfsDigest identifies the read-only root image the capture was taken
	// against. The template name alone does not: every node builds its own from
	// the Dockerfile, so the same name is different bytes on every node, and a
	// restore over the wrong bytes is silently broken rather than failed. Empty
	// on captures taken before this was recorded.
	RootfsDigest string `json:"rootfs_digest,omitempty"`
	// RootfsPath is the image the sandbox ran when that was not its template's
	// own (it was restored from a capture). A resume boots this, not the name.
	RootfsPath string `json:"rootfs_path,omitempty"`
	// CPUFeatures is the dispatch-relevant instruction set of the host that
	// took the capture. The guest's processes chose their code paths from it
	// before they were frozen, so a host without all of it kills the restored
	// guest with SIGILL; see checkCPU. Empty on captures taken before this was
	// recorded, and on a host whose features could not be read.
	CPUFeatures []string `json:"cpu_features,omitempty"`
	VCPUs       int      `json:"vcpus"`
	MemoryMB    int      `json:"memory_mb"`
	// Token is pushed to the resumed guest over vsock and used to reconnect envd.
	Token string `json:"token,omitempty"`
	// Egress is the sandbox's network policy, rebuilt into the fresh namespace on resume.
	Egress *egress.Rules `json:"egress,omitempty"`
	// Kind, SizeBytes and CreatedAt describe a named checkpoint and are unset on
	// the pause path, whose one snapshot needs no description.
	Kind      string    `json:"kind,omitempty"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// snapMetaOf is what a pause snapshot and a checkpoint both record about the
// sandbox they were taken from; a checkpoint adds its own description on top.
// VCPUs and MemoryMB stay 0: a restore uses the daemon record's resources.
func (m *Manager) snapMetaOf(sandboxID string, sb *Sandbox) sandboxSnapMeta {
	meta := sandboxSnapMeta{
		ID: sandboxID, OverlayPath: sb.OverlayPath,
		VMIP: sb.VMIP, TapName: sb.TapName, Template: sb.Template,
		Token: sb.Token, Egress: sb.Egress,
		CPUFeatures: hoststats.CPUFeatures(),
		RootfsPath:  sb.RootfsPath,
	}
	// The digest is of the image the guest actually runs, which for a sandbox
	// restored from a capture is not the one its template name resolves to.
	image := sb.RootfsPath
	if image == "" {
		image = m.rootfsPath(sb.Template)
	}
	// Best effort: an unreadable root image must not fail a capture of a sandbox
	// that is running perfectly well off it. A capture that records no digest is
	// let through by checkRootfs, exactly as a pre-digest one is.
	if sum, err := m.digestOf(image); err == nil {
		meta.RootfsDigest = sum
	} else {
		m.log.Warn("capture_rootfs_digest_failed",
			zap.String("sandbox_id", sandboxID), zap.String("template", sb.Template), zap.Error(err))
	}
	return meta
}

// SnapshotSandbox pauses the VM, writes state, mem and meta.json to destDir, stops
// the VM and removes the sandbox from the active set. The overlay and tap are kept
// for RestoreSandbox, so the daemon's later Destroy finds nothing and is a no-op;
// DiscardSnapshot is what finally removes them.
func (m *Manager) SnapshotSandbox(ctx context.Context, sandboxID, destDir string) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}

	if err := sb.Machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause VM: %w", err)
	}
	// Everything from here to the commit line can fail, and a pause that fails
	// must give the caller back the sandbox it asked to pause. Giving up the
	// machine first — dropping it from the active set, closing envd, cancelling
	// its timer — is what left a frozen, unreachable VM behind while the daemon's
	// record, which reverts to "running" on this error, still billed it as live.
	abort := func(format string, a ...any) error {
		if err := sb.Machine.ResumeVM(ctx); err != nil {
			m.log.Error("snapshot_abort_resume_failed",
				zap.String("sandbox_id", sandboxID), zap.Error(err))
		}
		return fmt.Errorf(format, a...)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return abort("mkdir snapshot dir: %w", err)
	}
	snap, err := m.beginMemorySnapshot(ctx, sb, destDir)
	if err != nil {
		return abort("create snapshot: %w", err)
	}
	if err := snap.finish(); err != nil {
		return abort("create snapshot: %w", err)
	}

	if err := writeSnapMeta(destDir, m.snapMetaOf(sandboxID, sb)); err != nil {
		return abort("write snapshot meta: %w", err)
	}

	// Committed: the image is whole, so the machine can go.
	m.mu.Lock()
	delete(m.active, sandboxID)
	m.mu.Unlock()
	m.removeOwner(sandboxID) // the VM is going; a resume records its new one
	if sb.stopTimer != nil {
		sb.stopTimer()
	}
	if sb.Envd != nil {
		sb.Envd.Close()
	}

	// Unlike Destroy, the overlay is kept. On the bridge the tap outlives the VMM
	// and is reused; a namespace is torn down so its slot is free while paused,
	// and resume builds a fresh one around the same baked tap name and address.
	sb.Machine.Stop(ctx)
	if sb.NetNS != "" {
		teardownNetNS(sb.NetNS)
	}

	m.log.Info("sandbox_snapshotted",
		zap.String("sandbox_id", sandboxID),
		zap.String("snapshot_dir", destDir),
		zap.String("vmip", sb.VMIP),
	)
	return nil
}

// RestoreSandbox resumes a VM from a per-sandbox snapshot, reusing the sandbox's
// own overlay and original VMIP, and registers it as active.
//
// The intermittent resume failure that used to be recorded here is explained
// and mitigated, and it is not caused by anything in this function. Measured on
// the reference host: the restored guest's kernel accepts the vsock connection,
// envd's accept loop accepts it too and spawns a handler -- and then the
// guest's Go runtime leaves that handler runnable without ever scheduling it
// for a minute or more, while the rest of the guest serves TCP normally, before
// recovering on its own. The old 15 s credPushDeadline was simply shorter than
// the stall, which is why a retry from the same snapshot failed identically
// rather than transiently. cred_push_ms below reports the stall whenever it
// happens. add-sandbox-standby/tasks.md has the goroutine dump.
//
// It is a property of this tier, not of standby: it reproduces on sandboxes
// that were never frozen, and its length is how long the VM had been alive when
// it was snapshotted rather than how long it spent frozen, 1:1 and with no
// ceiling -- a never-frozen 300-second-old sandbox stalled 303.9 s
// (fix-guest-thaw-clock-skew/tasks.md). Nor is it envd's: across a 303 s stall
// a 1 Hz timer in the guest executed one tick of ~303 while the guest's clock
// advanced the full 304 s, so what stops is timer-driven execution in the whole
// guest. It is intermittent and likelier the older the VM is: 0 of 3 restores
// at 30 s of age, 3 of 8 at 86 s, 4 of 5 at 300 s. The push waits a stall out
// (credPushStalledDeadline) rather than declaring the guest dead, so such a
// resume is slow rather than failed -- before that, a five-minute-old sandbox
// could not be resumed at all, because its snapshot stalls the same length on
// every attempt.
func (m *Manager) RestoreSandbox(ctx context.Context, sandboxID, srcDir string, vcpu, memMB int) (*Sandbox, error) {
	meta, err := readSnapMeta(srcDir)
	if err != nil {
		return nil, fmt.Errorf("read snapshot meta: %w", err)
	}
	rootfs := captureRootfs(srcDir)
	// A pause directory holds no image of its own; a sandbox that ran a
	// capture's image resumes on it, not on whatever the template name means here.
	if rootfs == "" && meta.RootfsPath != "" {
		if _, statErr := os.Stat(meta.RootfsPath); statErr == nil {
			rootfs = meta.RootfsPath
		}
	}
	if err := m.checkRestorable(meta, srcDir, rootfs); err != nil {
		return nil, err
	}
	origIP := net.ParseIP(meta.VMIP)
	if origIP == nil {
		return nil, fmt.Errorf("snapshot meta has invalid vmip %q", meta.VMIP)
	}
	if _, err := os.Stat(meta.OverlayPath); err != nil {
		return nil, fmt.Errorf("snapshot overlay missing: %w", err)
	}

	// The resuming VMM's uid is derived from this id, which need not be the id
	// the files were written under (the daemon's id differs from the VM layer's
	// on boot). These files belong to this sandbox alone, so chown is safe.
	uid := vmUID(m.config.NonRootVMM, sandboxID)
	statePath, memPath := filepath.Join(srcDir, "state"), filepath.Join(srcDir, "mem")
	for _, p := range []string{meta.OverlayPath, statePath, memPath} {
		if err := os.Chown(p, uid, uid); err != nil {
			return nil, fmt.Errorf("chown %s for resume: %w", filepath.Base(p), err)
		}
	}

	// Network: a fresh namespace around the baked identity, or on the bridge the
	// kept-alive tap with a fresh same-IP tap as fallback.
	var (
		nsName, nsPath string
		tapName        = meta.TapName
		nodeAddr       = origIP.String() + ":6001"
		teardown       func()
	)
	if m.config.NetNS {
		ns, err := setupNetNS(sandboxID, uid, meta.TapName, origIP, meta.Egress, m.log)
		if err != nil {
			return nil, fmt.Errorf("netns: %w", err)
		}
		nsName, nsPath, tapName, nodeAddr = ns.Name, ns.NetNSPath, ns.TapName, ns.NodeAddr
		teardown = func() { teardownNetNS(ns.Name) }
	} else {
		netw := &NetworkSetup{TapName: meta.TapName, VMIP: origIP}
		if meta.TapName == "" || !tapExists(meta.TapName) {
			n, err := SetupVMNetworkWithIP(origIP, uid)
			if err != nil {
				return nil, fmt.Errorf("network (same IP): %w", err)
			}
			netw = n
		}
		tapName = netw.TapName
		teardown = func() { TeardownVMNetwork(netw.TapName) }
	}

	p := restoreParams{
		SandboxID: sandboxID, Template: meta.Template, RootfsPath: rootfs,
		TapName: tapName, VMIP: origIP, NodeAddr: nodeAddr,
		OverlayPath: meta.OverlayPath, StatePath: statePath, MemPath: memPath,
		VCPU: vcpu, MemMB: memMB,
		NetNSPath: nsPath, NetNSName: nsName,
		Token: meta.Token, BootType: "snapshot",
		LazyMem: lazyMemEnabled(),
	}
	t0 := time.Now()
	machine, err := m.bootSnapshot(p)
	if err != nil {
		teardown()
		return nil, fmt.Errorf("restore snapshot: %w", err)
	}

	// Push the credential over this VM's vsock before anyone is told it is ready.
	// An empty token is refused rather than hashed: HashHex("") is a well-formed
	// credential envd would accept and nothing could ever satisfy.
	if meta.Token == "" {
		machine.Stop(ctx)
		teardown()
		return nil, fmt.Errorf("resume %s: %w: snapshot %s carries no access token "+
			"(taken with envd auth off?), and a restored guest cannot be given one it does not have",
			sandboxID, ErrCredentialPush, srcDir)
	}
	tPush := time.Now()
	if err := deliverCredential(machine.ChrootRoot(), sandboxID, sbtoken.HashHex(meta.Token), credPushDeadline); err != nil {
		machine.Stop(ctx)
		teardown()
		return nil, fmt.Errorf("resume %s: %w", sandboxID, err)
	}

	m.log.Info("resume_cred_push_ok",
		zap.String("sandbox_id", sandboxID), zap.Int64("cred_push_ms", time.Since(tPush).Milliseconds()))

	sb, err := m.attachRestored(p, machine, t0)
	if err != nil {
		machine.Stop(ctx)
		teardown()
		return nil, fmt.Errorf("envd not ready after resume: %w", err)
	}

	if sb.NetNS == "" {
		// Resume may have replaced the tap; keep the ownership record in step.
		m.saveTap(sandboxID, sb.TapName)
	}
	// The first resume re-keys the sandbox from the boot-time VM id to the
	// daemon's; the old key's records would otherwise outlive the sandbox, since
	// nothing is keyed by it any more. Its lineage directory is deliberately left
	// alone: removing it here broke the second pause/resume cycle on the node
	// (resume then fails with a credential-push timeout), so the memory lineage
	// is still reachable from the boot-time key.
	if meta.ID != "" && meta.ID != sandboxID {
		m.removeToken(meta.ID)
		m.removeEndpoint(meta.ID)
		m.removeTap(meta.ID)
	}

	m.log.Info("sandbox_resumed",
		zap.String("sandbox_id", sandboxID),
		zap.String("vmip", sb.VMIP),
		zap.Int64("resume_ms", sb.BootMs),
	)
	return sb, nil
}

// restoreParams is what booting a sandbox from a snapshot image needs. A fork
// and a resume fill it from different places - a fork mints a token and boots a
// copy of the image, a resume reuses the captured token and the sandbox's own
// files - but the boot and the registration around it are the same.
type restoreParams struct {
	// LazyMem restores memory through the Uffd backend; see FirecrackerConfig.
	LazyMem bool
	// MemSource serves memory with no file behind it; see FirecrackerConfig.
	MemSource runtime.MemSource
	SandboxID string
	Template  string
	// RootfsPath is the capture's own root image when it carries one; "" leaves
	// the template name to resolve it.
	RootfsPath  string
	TapName     string
	VMIP        net.IP
	OverlayPath string
	StatePath   string
	MemPath     string
	VCPU        int
	MemMB       int
	NetNSPath   string // "" on the shared bridge
	NetNSName   string // "" on the shared bridge
	NodeAddr    string
	Token       string // "" only when envd auth is off
	BootType    string
}

// bootSnapshot starts the VMM on the image p names. The caller owns the network
// and the files and must release them if this fails.
func (m *Manager) bootSnapshot(p restoreParams) (FirecrackerMachine, error) {
	return restoreFirecrackerSnapshot(FirecrackerConfig{
		SandboxID:   p.SandboxID,
		Template:    p.Template,
		RootfsPath:  p.RootfsPath,
		TapName:     p.TapName,
		VMIP:        p.VMIP,
		OverlayPath: p.OverlayPath,
		StatePath:   p.StatePath,
		MemPath:     p.MemPath,
		LazyMem:     p.LazyMem,
		MemSource:   p.MemSource,
		VCPUs:       p.VCPU,
		MemoryMB:    p.MemMB,
		NetNS:       p.NetNSPath,
		Config:      m.config,
	})
}

// attachRestored connects envd on a machine the caller has already given its
// credential, and registers the sandbox. bootStart is when the restore began.
// On error the machine is still the caller's to stop.
func (m *Manager) attachRestored(p restoreParams, machine FirecrackerMachine, bootStart time.Time) (*Sandbox, error) {
	// ConnectWithToken's readiness check is authenticated, so a credential that
	// did not arrive fails the restore here instead of every later call.
	envdClient, err := envd.ConnectWithToken(p.NodeAddr, p.Token, 30*time.Second)
	if err != nil {
		return nil, err
	}
	_, cancel := context.WithCancel(context.Background())
	sb := &Sandbox{
		ID:          p.SandboxID,
		Template:    p.Template,
		RootfsPath:  p.RootfsPath,
		Machine:     machine,
		Envd:        envdClient,
		OverlayPath: p.OverlayPath,
		TapName:     p.TapName,
		VMIP:        p.VMIP.String(),
		NodeAddr:    p.NodeAddr,
		NetNS:       p.NetNSName,
		MemBase:     p.MemPath,
		BootType:    p.BootType,
		BootMs:      time.Since(bootStart).Milliseconds(),
		CreatedAt:   time.Now(),
		Token:       p.Token,
		stopTimer:   cancel, // the daemon owns TTL
	}
	// Take our own link to the image straight away. p.MemPath is inside a
	// capture directory the caller owns and may delete — `checkpoint rm` after a
	// restore did exactly that, and every later memory operation on a healthy
	// sandbox then failed on a missing file. Warn rather than fail: the worst
	// case is the path we would have kept anyway.
	if base, err := m.adoptMemBase(p.SandboxID, p.MemPath); err != nil {
		m.log.Warn("mem_base_adopt_failed",
			zap.String("sandbox_id", p.SandboxID), zap.String("mem", p.MemPath), zap.Error(err))
	} else {
		sb.MemBase = base
	}
	m.mu.Lock()
	m.active[p.SandboxID] = sb
	m.mu.Unlock()
	m.saveToken(p.SandboxID, p.Token)
	m.saveEndpoint(p.SandboxID, p.NodeAddr)

	// The guest's wall clock stopped when the image was written, and there is no
	// NTP or RTC inside to notice. Here rather than in each caller: a resume, a
	// fork and a machine stamped from a named snapshot all come out of a memory
	// image, and the one a week old is the create-from-snapshot case. Best-effort
	// and after envd answers: a stale clock is worse than a correct one and far
	// better than a restore that failed.
	if err := deliverGuestClock(machine.ChrootRoot(), p.SandboxID); err != nil {
		m.log.Warn("restore_clock_push_failed",
			zap.String("sandbox_id", p.SandboxID), zap.String("boot_type", p.BootType), zap.Error(err))
	}
	return sb, nil
}

func writeSnapMeta(dir string, meta sandboxSnapMeta) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
}

func readSnapMeta(dir string) (sandboxSnapMeta, error) {
	var meta sandboxSnapMeta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return meta, err
	}
	return meta, json.Unmarshal(b, &meta)
}

// DiscardSnapshot releases what SnapshotSandbox kept for a resume that will never
// come: the overlay and lineage, a bridge-mode tap and the token/endpoint/tap
// records. Works from meta.json alone so it also covers a sandbox paused by a
// previous run of this daemon. Nothing there is not an error.
func (m *Manager) DiscardSnapshot(srcDir string) error {
	meta, err := readSnapMeta(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read snapshot meta: %w", err)
	}
	deleteOverlay(meta.OverlayPath)
	m.removeLineage(meta.ID, meta.OverlayPath)
	if meta.ID != "" {
		m.removeToken(meta.ID)
		m.removeEndpoint(meta.ID)
		m.removeTap(meta.ID)
	}
	// A namespace was torn down at pause; on the bridge the tap was kept alive.
	if !m.config.NetNS && meta.TapName != "" {
		TeardownVMNetwork(meta.TapName)
	}
	m.log.Info("snapshot_discarded", zap.String("sandbox_id", meta.ID), zap.String("snapshot_dir", srcDir))
	return nil
}
