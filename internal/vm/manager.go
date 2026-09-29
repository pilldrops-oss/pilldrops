//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

type Config struct {
	SnapshotDir    string
	OverlayDir     string
	FirecrackerBin string
	JailerBin      string
	KernelPath     string
	Templates      []string
	MaxConcurrent  int // max simultaneous creates
	MaxActive      int // max total running sandboxes
	MemoryMB       int // default guest RAM per VM (0 means 1024; 512 OOM-kills V8/CPython)
	VCPUs          int // default vCPUs per VM (0 means 1)
	// Resources holds per-template defaults from the catalog; MemoryMB/VCPUs
	// apply when a template has no entry.
	Resources map[string]runtime.Resources
	// EnableSnapshots turns on warm restore from a golden snapshot; off means every create cold boots.
	// The template's baked envd must have the vsock credential channel, or golden
	// VMs never reach envd-ready and the node silently serves cold boots.
	EnableSnapshots bool
	// EnvdAuth mints a per-sandbox envd token so a neighbour VM cannot drive this
	// sandbox's envd. Only the hash enters the guest: on the kernel cmdline for a
	// cold boot, pushed over vsock after resume for a restore.
	EnvdAuth bool
	// NonRootVMM runs the jailer/Firecracker VMM as a per-sandbox unprivileged uid.
	// The zero value is root with full capabilities, so every constructor must set it.
	NonRootVMM bool
	// NetNS boots every sandbox in its own network namespace instead of the shared
	// fcbr0 bridge. The node writes the egress firewall into each namespace, since
	// host FORWARD/INPUT rules never see the post-SNAT traffic.
	NetNS bool
}

type Sandbox struct {
	ID       string
	Template string
	// RootfsPath is the read-only root image this VM runs when it is not the
	// one its template name resolves to here: a sandbox restored from a
	// capture (fork, named snapshot, move) runs the capture's image, and a
	// later pause must resume it on that image, not on this node's own build.
	RootfsPath  string
	UserID      string
	Machine     FirecrackerMachine
	Envd        *envd.Client
	OverlayPath string
	TapName     string
	VMIP        string
	BootType    string // "snapshot" | "cold"
	BootMs      int64
	// NetNS is the sandbox's network namespace, "" on the shared-bridge path.
	NetNS string
	// NodeAddr is the "<ip>:6001" the node dials to reach this sandbox's envd.
	// Under a namespace that is the slot's own address, which the in-namespace
	// DNAT maps onto the fixed guest IP for every port, not only 6001; on the
	// shared bridge it is VMIP's. Everything the host dials goes to HostIP.
	NodeAddr string
	// MemBase is the memory file this VM tracks dirty pages against (the file
	// it was restored from, or its last full snapshot); "" means no lineage
	// yet and the next snapshot must be full.
	MemBase   string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Token is the per-sandbox envd access token, "" when auth is off.
	Token string
	// Egress is the policy this sandbox booted under, kept so a fork inherits it.
	Egress    *egress.Rules
	stopTimer context.CancelFunc
}

type Manager struct {
	config        Config
	active        map[string]*Sandbox
	createLimiter chan struct{} // concurrent create cap
	// recoverable holds VMs found running at startup that have an adoption
	// record: kept alive for the daemon to claim (adopt.go), keyed by vm id.
	recoverable  map[string]*OrphanProcess
	diskPressure bool // set by monitor when disk >80%
	log          *zap.Logger
	mu           sync.RWMutex

	// rootfsSums memoises root-image digests by path+size+mtime; its own lock so
	// hashing a 200 MB image never holds the lock the active set is read under.
	sumMu      sync.Mutex
	rootfsSums map[string]string
}

// checkHostDeps fails at startup, naming the missing binary, instead of letting
// every create die late inside the SDK. Bridge mode's iptables is not checked
// here; preflight already detects a missing setup-host.sh.
func checkHostDeps(cfg Config) error {
	if _, err := os.Stat(cfg.JailerBin); err != nil {
		return fmt.Errorf("host dependency missing: jailer binary %q: %w — every VM boot would fail; "+
			"install firecracker's jailer or set JAILER_BIN", cfg.JailerBin, err)
	}
	// AF_UNIX paths cap at 108 bytes; the API socket sits at
	// <jailer>/firecracker/<id>/root/run/<id>.sock and a too-long path makes
	// every boot die in the SDK as "did not create API socket".
	const sampleID = "sb-0000000000000-00000000"
	if sock := filepath.Join(jailerChrootDir(), sampleID, "root", "run", sampleID+".sock"); len(sock) > 107 {
		return fmt.Errorf("FC_JAILER_DIR %q is too deep: the VM API socket path %q would exceed the "+
			"107-byte unix socket limit; use a shorter directory", jailerBaseDir, sock)
	}
	if cfg.NetNS {
		for _, bin := range []string{"nft", "nsenter"} {
			if _, err := exec.LookPath(bin); err != nil {
				return fmt.Errorf("host dependency missing: %q: %w — netns mode cannot write or apply "+
					"the per-sandbox firewall without it; install it (nft: package nftables, nsenter: "+
					"util-linux) or fall back to the shared bridge explicitly with FC_NETNS=0", bin, err)
			}
		}
	}
	return nil
}

func NewManager(cfg Config, log *zap.Logger) (*Manager, error) {
	m := &Manager{
		config:        cfg,
		active:        make(map[string]*Sandbox),
		recoverable:   make(map[string]*OrphanProcess),
		createLimiter: make(chan struct{}, cfg.MaxConcurrent),
		log:           log,
	}

	if err := checkHostDeps(cfg); err != nil {
		return nil, err
	}

	// The jailer SIGABRTs canonicalizing a missing chroot base.
	if err := os.MkdirAll(jailerChrootDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create jailer chroot base: %w", err)
	}

	// A snapshot failure is never fatal: the node degrades to cold boot. Existing
	// goldens are sealed and their taps reserved before any create; missing ones
	// are built in the background, one at a time, while creates cold boot.
	if cfg.EnableSnapshots {
		var missing []string
		for _, tmpl := range cfg.Templates {
			ok, err := validateSnapshots(tmpl, cfg, log)
			if err != nil {
				log.Warn("snapshot_init_failed_using_coldboot",
					zap.String("template", tmpl),
					zap.Error(err),
				)
				continue
			}
			if !ok {
				missing = append(missing, tmpl)
			}
		}
		if len(missing) > 0 {
			log.Info("golden_snapshots_building_in_background", zap.Strings("templates", missing))
			go func() {
				for _, tmpl := range missing {
					if err := RebuildSnapshots(tmpl, cfg, log); err != nil && !errors.Is(err, errGoldenSkipped) {
						log.Warn("snapshot_init_failed_using_coldboot",
							zap.String("template", tmpl),
							zap.Error(err),
						)
					}
				}
			}()
		}
	} else {
		log.Info("snapshots_disabled_coldboot_mode", zap.Strings("templates", cfg.Templates))
	}

	m.RecoverOrphans()

	// Leftover taps make every create fail with "TUNSETIFF: device busy".
	if deleted := m.cleanupStaleTaps(); deleted > 0 {
		log.Info("cleaned_stale_taps", zap.Int("count", deleted))
	}

	// Runs in both modes so a bridge-mode boot still reclaims a previous netns run's leftovers.
	if removed := m.reclaimNetNS(); removed > 0 {
		log.Info("reclaimed_stale_netns", zap.Int("count", removed))
	}
	// The host table is host-global, so a bridge-mode boot must never remove it:
	// a side-car daemon would de-egress every sandbox of a co-hosted netns daemon.
	if cfg.NetNS {
		if err := EnsureNetNSHostRules(); err != nil {
			return nil, fmt.Errorf("netns host rules: %w", err)
		}
	}

	return m, nil
}

type CreateRequest struct {
	Template string
	UserID   string
	Timeout  time.Duration
	VCPUs    int // 0 means template/global default
	MemoryMB int // 0 means template/global default
	// StorageMB is the writable-disk cap, already resolved by internal/sandbox; 0 means DefaultOverlayMB.
	StorageMB int
	// Egress is the sandbox's egress policy; nil is unrestricted. Only the netns
	// path can enforce one, and a boot that cannot fails rather than pretends.
	Egress *egress.Rules
}

// defaultResources returns the template's default vCPU/RAM, falling back to the
// global default and finally 1 vCPU / 1024 MiB.
func defaultResources(cfg Config, template string) (int, int) {
	def := cfg.Resources[template]
	vcpu, mem := def.VCPUs, def.MemoryMB
	if vcpu <= 0 {
		vcpu = cfg.VCPUs
	}
	if vcpu <= 0 {
		vcpu = 1
	}
	if mem <= 0 {
		mem = cfg.MemoryMB
	}
	if mem <= 0 {
		mem = 1024
	}
	return vcpu, mem
}

// resolveResources returns the effective vCPU/RAM; a non-zero request value overrides the template default.
func (m *Manager) resolveResources(template string, reqVCPU, reqMem int) (int, int) {
	dv, dm := defaultResources(m.config, template)
	vcpu, mem := reqVCPU, reqMem
	if vcpu <= 0 {
		vcpu = dv
	}
	if mem <= 0 {
		mem = dm
	}
	return vcpu, mem
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Sandbox, error) {
	m.mu.RLock()
	diskPressure := m.diskPressure
	activeCount := len(m.active)
	m.mu.RUnlock()

	if diskPressure {
		return nil, ErrDiskPressure
	}

	if activeCount >= m.config.MaxActive {
		return nil, ErrServerFull
	}

	release, ok := m.acquireCreateSlot()
	if !ok {
		return nil, ErrTooManyRequests
	}
	defer release()

	// Validate before the join: filepath.Join cleans "../../../etc" into a path
	// outside SnapshotDir that Stat would happily probe.
	if err := runtime.ValidateTemplateName(req.Template); err != nil {
		return nil, err
	}
	rootfs := filepath.Join(m.config.SnapshotDir, req.Template, "rootfs.squashfs")
	if _, err := os.Stat(rootfs); err != nil {
		return nil, runtime.ErrTemplateUnknown
	}

	sandboxID := generateSandboxID()
	start := time.Now()

	// Only the hash reaches the guest.
	var token, tokenHash string
	if m.config.EnvdAuth {
		token, tokenHash = sbtoken.Mint()
	}

	vm, bootType, err := m.acquireVM(ctx, req, sandboxID, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("acquire VM: %w", err)
	}
	bootMs := time.Since(start).Milliseconds()

	envdClient, err := envd.ConnectWithToken(vm.VsockPath, token, 10*time.Second)
	if err != nil {
		vm.Destroy()
		return nil, fmt.Errorf("envd not ready: %w", err)
	}

	timerCtx, cancel := context.WithCancel(context.Background())
	// Timeout <= 0 means the daemon owns TTL: zero ExpiresAt and no backstop timer.
	// A 24h backstop here once killed persistent workstations behind the daemon's back.
	expiresAt := time.Time{}
	if req.Timeout > 0 {
		expiresAt = time.Now().Add(req.Timeout)
	}
	sb := &Sandbox{
		ID:          sandboxID,
		Template:    req.Template,
		UserID:      req.UserID,
		Machine:     vm.Machine,
		Envd:        envdClient,
		OverlayPath: vm.OverlayPath,
		TapName:     vm.TapName,
		VMIP:        vm.VMIP,
		NodeAddr:    vm.VsockPath,
		NetNS:       vm.NetNS,
		MemBase:     vm.MemBase,
		BootType:    bootType,
		BootMs:      bootMs,
		CreatedAt:   time.Now(),
		ExpiresAt:   expiresAt,
		Token:       token,
		Egress:      req.Egress,
		stopTimer:   cancel,
	}

	// Host-side auto-destroy, so the sandbox dies even if envd hangs.
	if req.Timeout > 0 {
		go func() {
			// NewTimer + Stop, not time.After: a time.After timer stays on the
			// heap until it fires, long after most sandboxes are destroyed.
			t := time.NewTimer(req.Timeout)
			defer t.Stop()
			select {
			case <-t.C:
				m.log.Info("sandbox_timeout", zap.String("sandbox_id", sandboxID))
				m.Destroy(context.Background(), sandboxID)
			case <-timerCtx.Done():
			}
		}()
	}

	m.mu.Lock()
	m.active[sandboxID] = sb
	m.mu.Unlock()

	// Persisted so orphan recovery after a restart can reconnect envd.
	m.saveToken(sandboxID, token)
	m.saveEndpoint(sandboxID, vm.VsockPath)
	if sb.NetNS == "" {
		// Lets the startup sweep tell this daemon's stale taps from a co-hosted
		// daemon's live ones. A netns tap is reclaimed with its namespace.
		m.saveTap(sandboxID, sb.TapName)
	}

	m.log.Info("sandbox_created",
		zap.String("sandbox_id", sandboxID),
		zap.String("template", req.Template),
		zap.String("boot_type", bootType),
		zap.Int64("boot_ms", bootMs),
		zap.String("user_id", req.UserID),
	)

	return sb, nil
}

// restoreServes reports whether a golden snapshot can serve the request. A golden
// is taken at the template's default vCPU/RAM and disk (goldenStorageMB), so
// any other size, storage included, must cold boot or the cap is bypassed.
func restoreServes(wantVCPU, defVCPU, wantMem, defMem, storageMB, goldenMB int) bool {
	return wantVCPU == defVCPU && wantMem == defMem &&
		(storageMB <= 0 || storageMB == goldenMB)
}

// goldenStorageMB is the disk a template's golden is built with: the template's
// own default, so an ordinary create restores. It was DefaultOverlayMB (20 GB)
// for every template while creates ask for the template default (1 GB for
// base), which made every default create ineligible and every create a cold
// boot, measured on the four-node fleet.
func goldenStorageMB(cfg Config, template string) int {
	if mb := cfg.Resources[template].StorageMB; mb > 0 {
		return mb
	}
	return DefaultOverlayMB
}

func (m *Manager) acquireVM(_ context.Context, req CreateRequest, sandboxID, tokenHash string) (*RestoredVM, string, error) {
	template := req.Template

	wantVCPU, wantMem := m.resolveResources(template, req.VCPUs, req.MemoryMB)
	defVCPU, defMem := m.resolveResources(template, 0, 0)

	// On a deployment with per-plan storage caps most creates cold boot, since
	// goldens exist only at DefaultOverlayMB. The warn makes that visible.
	goldenMB := goldenStorageMB(m.config, template)
	restoreEligible := restoreServes(wantVCPU, defVCPU, wantMem, defMem, req.StorageMB, goldenMB)
	if !restoreEligible && m.config.EnableSnapshots {
		m.log.Warn("restore_ineligible_custom_size",
			zap.String("template", template),
			zap.Int("want_vcpu", wantVCPU), zap.Int("def_vcpu", defVCPU),
			zap.Int("want_mem", wantMem), zap.Int("def_mem", defMem),
			zap.Int("storage_mb", req.StorageMB), zap.Int("golden_storage_mb", goldenMB))
	}

	// A token hash does not disqualify restore; it is pushed over vsock after resume.
	// Do not wrap this in singleflight: it deduplicates rather than serialises and
	// would hand two tenants the same VM. claimGoldenTap covers the only critical window.
	if restoreEligible && m.config.EnableSnapshots {
		vm, err := RestoreFromSnapshot(template, sandboxID, m.config, m.log, tokenHash, req.Egress)
		if err == nil {
			return vm, "snapshot", nil
		}
		if !errors.Is(err, ErrNoGolden) {
			m.log.Warn("snapshot_restore_failed", zap.String("template", template), zap.Error(err))
		}
	}

	vm, err := ColdBoot(template, sandboxID, m.config, m.log, wantVCPU, wantMem, req.StorageMB, tokenHash, req.Egress)
	if err != nil {
		return nil, "", fmt.Errorf("all boot methods failed: %w", err)
	}
	return vm, "cold", nil
}

var ErrForbidden = errors.New("forbidden: sandbox belongs to another user")

// AuthorizeSandbox checks that the sandbox exists and belongs to the given user.
func (m *Manager) AuthorizeSandbox(sandboxID, userID string) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	if sb.UserID != "" && userID != "" && sb.UserID != userID {
		return ErrForbidden
	}
	return nil
}

func (m *Manager) Exec(ctx context.Context, sandboxID, cmd, cwd string, timeout time.Duration) (*envd.ExecResult, error) {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrSandboxNotFound
	}
	return sb.Envd.Execute(ctx, cmd, cwd, timeout)
}

// ExecStream runs a command and relays each envd output frame to onFrame as it arrives.
func (m *Manager) ExecStream(ctx context.Context, sandboxID, cmd, cwd string, timeout time.Duration, onFrame func(envd.StreamFrame)) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	return sb.Envd.ExecuteStream(ctx, cmd, cwd, timeout, onFrame)
}

// ExecDetached runs a start command that outlives the exec RPC; envd does not reap it.
func (m *Manager) ExecDetached(ctx context.Context, sandboxID, cmd, cwd string, timeout time.Duration) (*envd.ExecResult, error) {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrSandboxNotFound
	}
	return sb.Envd.ExecuteDetached(ctx, cmd, cwd, timeout)
}

// GetSandboxInfo returns public metadata for a single sandbox.
func (m *Manager) GetSandboxInfo(sandboxID string) (SandboxInfo, error) {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return SandboxInfo{}, ErrSandboxNotFound
	}
	return sb.info(), nil
}

// info is the reportable projection of a sandbox: the fields a caller outside
// this package may see, and nothing the engine alone owns.
func (sb *Sandbox) info() SandboxInfo {
	return SandboxInfo{
		ID:        sb.ID,
		Template:  sb.Template,
		UserID:    sb.UserID,
		BootType:  sb.BootType,
		BootMs:    sb.BootMs,
		VMIP:      sb.VMIP,
		CreatedAt: sb.CreatedAt,
		ExpiresAt: sb.ExpiresAt,
	}
}

// ListUserSandboxes returns all active sandboxes belonging to a user.
func (m *Manager) ListUserSandboxes(userID string) []SandboxInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SandboxInfo, 0)
	for _, sb := range m.active {
		if sb.UserID == userID {
			out = append(out, sb.info())
		}
	}
	return out
}

// WriteFile writes a file inside the sandbox via envd.
func (m *Manager) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	return sb.Envd.WriteFile(ctx, path, data)
}

// ReadFile reads a file from the sandbox via envd.
func (m *Manager) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrSandboxNotFound
	}
	return sb.Envd.ReadFile(ctx, path)
}

// RefreshTTL extends a sandbox's expiry time by the given duration from now.
func (m *Manager) RefreshTTL(sandboxID string, extension time.Duration) (time.Time, error) {
	m.mu.Lock()
	sb, ok := m.active[sandboxID]
	if !ok {
		m.mu.Unlock()
		return time.Time{}, ErrSandboxNotFound
	}
	sb.ExpiresAt = time.Now().Add(extension)
	newExpiry := sb.ExpiresAt
	m.mu.Unlock()
	return newExpiry, nil
}

func (m *Manager) Destroy(ctx context.Context, sandboxID string) error {
	m.mu.Lock()
	sb, ok := m.active[sandboxID]
	if ok {
		delete(m.active, sandboxID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	m.removeOwner(sandboxID)

	if sb.stopTimer != nil { // recovered/restored sandboxes may have no timer
		sb.stopTimer()
	}
	sb.Machine.Stop(ctx)
	sb.Envd.Close()
	cleanJailerRoot(sb.Machine.ChrootRoot())
	// A restored sandbox's image sits in its own directory, which os.Remove would leak.
	deleteOverlay(sb.OverlayPath)
	m.removeLineage(sandboxID, sb.OverlayPath)
	if sb.NetNS != "" {
		teardownNetNS(sb.NetNS)
	} else {
		TeardownVMNetwork(sb.TapName)
	}
	m.removeToken(sandboxID)
	m.removeEndpoint(sandboxID)
	m.removeTap(sandboxID)

	m.log.Info("sandbox_destroyed",
		zap.String("sandbox_id", sandboxID),
		zap.Duration("lifetime", time.Since(sb.CreatedAt)),
		zap.String("boot_type", sb.BootType),
	)
	return nil
}

// RecoverOrphans kills Firecracker processes left by previous runs of this daemon.
// /proc is host-wide, so a VM with no record in this daemon's state dir is a
// co-hosted daemon's or a template build's and is left alone.
func (m *Manager) RecoverOrphans() {
	m.recoverOrphanProcs(findFirecrackerProcesses())
	m.sweepLeakedChroots()
}

func (m *Manager) recoverOrphanProcs(procs []*OrphanProcess) {
	for _, proc := range procs {
		if _, exists := m.active[proc.SandboxID]; exists {
			continue
		}
		// A VM a daemon sandbox owned survives its daemon's crash: hold it for
		// the daemon's restore to adopt rather than killing it (adopt.go).
		if rec, ok := m.readAdopt(proc.SandboxID); ok {
			m.recoverable[proc.SandboxID] = proc
			m.log.Info("vm_held_for_adoption", zap.String("vm_id", proc.SandboxID), zap.String("owner", rec.Owner))
			continue
		}
		if !m.knowsSandbox(proc.SandboxID) {
			pid := 0
			if proc.Machine != nil {
				pid = proc.Machine.Pid()
			}
			m.log.Info("skipping_unowned_vm",
				zap.String("sandbox_id", proc.SandboxID),
				zap.Int("pid", pid),
				zap.String("reason", "no record in this daemon's state dir — another sandboxd's VM, or a template-build VM; not ours to kill"))
			continue
		}
		// The /proc scan yields a sandbox id but not the VM's address or overlay,
		// so both come from this daemon's records.
		token := m.loadToken(proc.SandboxID)
		addr := m.loadEndpoint(proc.SandboxID)
		proc.VsockPath = addr
		if proc.OverlayPath == "" {
			proc.OverlayPath = filepath.Join(m.config.OverlayDir, proc.SandboxID+".ext4")
		}

		// Reconnect only to log why the VM goes away, then kill it either way.
		// Adopting it would strand it: the daemon addresses sandboxes by its own
		// id, which is not persisted here, so nothing above could reach or destroy it.
		var reason error
		dial := func() (*envd.Client, error) {
			// ConnectWithToken refuses an empty token.
			if token == "" {
				return envd.Connect(addr, 3*time.Second)
			}
			return envd.ConnectWithToken(addr, token, 3*time.Second)
		}
		if addr == "" {
			reason = fmt.Errorf("no persisted envd endpoint")
		} else if c, err := dial(); err != nil {
			reason = fmt.Errorf("envd unreachable: %w", err)
		} else {
			c.Close()
			reason = errors.New("alive, but the daemon cannot re-address it after a restart")
		}
		killErr := proc.Kill()
		m.log.Info("killing_orphan",
			zap.String("sandbox_id", proc.SandboxID),
			zap.String("endpoint", addr),
			zap.String("reason", reason.Error()),
			zap.Error(killErr))
		if killErr != nil {
			// Never unlink the backing files of a VM that is still running.
			m.log.Warn("orphan_still_running_after_kill",
				zap.String("sandbox_id", proc.SandboxID), zap.Error(killErr))
			continue
		}
		deleteOverlay(proc.OverlayPath)
		cleanJailerRoot(filepath.Join(jailerChrootDir(), proc.SandboxID, "root"))
		// The tap outlives its VMM; take it down before dropping the record.
		if tap := m.loadTap(proc.SandboxID); tap != "" {
			TeardownVMNetwork(tap)
		}
		m.removeToken(proc.SandboxID)
		m.removeEndpoint(proc.SandboxID)
		m.removeTap(proc.SandboxID)
	}
}

// knowsSandbox reports whether this daemon's state dir has any record of the
// sandbox. Every create writes at least an endpoint record; template-build VMs
// are deliberately unrecorded.
func (m *Manager) knowsSandbox(id string) bool {
	return m.loadEndpoint(id) != "" || m.loadToken(id) != "" || m.loadTap(id) != ""
}

// chrootSweepGrace protects a booting VM's chroot: the jailer creates it before
// the process is visible in /proc or the id is registered in m.active.
const chrootSweepGrace = 3 * time.Minute

// sweepLeakedChroots removes chroot dirs that are neither active nor backing a
// live VMM and are older than chrootSweepGrace. Bails when /proc is unreadable
// rather than treating that as "nothing is live".
func (m *Manager) sweepLeakedChroots() {
	entries, err := os.ReadDir(jailerChrootDir())
	if err != nil {
		return
	}
	live, ok := liveJailerIDs()
	if !ok {
		m.log.Warn("skipping_leaked_chroot_sweep", zap.String("reason", "proc_unreadable"))
		return
	}
	var names []string
	modTimes := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		names = append(names, e.Name())
		// A stat failure leaves a zero mtime, which reads as infinitely old.
		if info, err := e.Info(); err == nil {
			modTimes[e.Name()] = info.ModTime()
		}
	}

	m.mu.RLock()
	dead := sweepDecision(names, modTimes, time.Now(), live, func(id string) bool {
		_, active := m.active[id]
		return active
	})
	m.mu.RUnlock()

	for _, id := range dead {
		m.log.Info("sweeping_leaked_jailer_chroot", zap.String("id", id))
		cleanJailerRoot(filepath.Join(jailerChrootDir(), id, "root"))
	}
}

func isSweepEligible(id string, modTime, now time.Time, live map[string]bool, active bool) bool {
	if active || live[id] {
		return false
	}
	return now.Sub(modTime) >= chrootSweepGrace
}

// sweepDecision returns the chroot ids safe to remove; pure so it is unit-testable.
func sweepDecision(entryNames []string, modTimes map[string]time.Time, now time.Time, live map[string]bool, active func(string) bool) []string {
	var dead []string
	for _, id := range entryNames {
		if isSweepEligible(id, modTimes[id], now, live, active(id)) {
			dead = append(dead, id)
		}
	}
	return dead
}

// removeLineage drops the memory-lineage directories a sandbox owns: the one
// under its current key and, for a resumed sandbox, the one under the boot-time
// VM id — a resume re-keys the sandbox but not its overlay, which keeps that id
// in its name. Only safe once the VMM is down: dropping the boot-time lineage
// while the sandbox is live breaks its next resume.
func (m *Manager) removeLineage(id, overlayPath string) {
	os.RemoveAll(m.lineageDir(id))
	if boot := overlayOwnerID(overlayPath); boot != "" && boot != id {
		os.RemoveAll(m.lineageDir(boot))
	}
}

// overlayOwnerID is the id an overlay path is named for: the directory name
// under restore/, else the file name without .ext4.
func overlayOwnerID(path string) string {
	if path == "" {
		return ""
	}
	dir := filepath.Dir(path)
	if filepath.Base(filepath.Dir(dir)) == restoreOverlaySubdir {
		return filepath.Base(dir)
	}
	return strings.TrimSuffix(filepath.Base(path), ".ext4")
}

func (m *Manager) SetDiskPressure(v bool) {
	m.mu.Lock()
	m.diskPressure = v
	m.mu.Unlock()
}

// DiskPressure reports the watchdog's flag. Exported for callers that make a
// large write BEFORE Create is reached: materialising a named snapshot means
// fetching its memory image, its state and its disk out of the artifact store,
// which is larger than anything Create then does, and it happens a layer above
// every guard in this package.
func (m *Manager) DiskPressure() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.diskPressure
}

func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.active)
}

// Alive reports whether the sandbox's VMM process still exists. A sandbox whose
// pid was never recorded is treated as alive so a gap during boot or adopt
// cannot trigger a spurious reap.
func (m *Manager) Alive(sandboxID string) bool {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok || sb.Machine == nil {
		return false
	}
	pid := sb.Machine.Pid()
	if pid <= 0 {
		return true
	}
	return syscall.Kill(pid, 0) == nil
}

func (m *Manager) ActiveSandboxes() []SandboxInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SandboxInfo, 0, len(m.active))
	for _, sb := range m.active {
		out = append(out, sb.info())
	}
	return out
}

func (m *Manager) SetMaxConcurrent(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config.MaxConcurrent = n
	m.createLimiter = make(chan struct{}, n)
}

// createSlotWait is how long a create queues for a boot slot before the node
// says it is busy. Refusing at once failed 30 of 40 concurrent creates in
// 0.1 s on one node, when a restore boots in ~40 ms and every one of them
// would have been served in well under this. A var so tests do not wait.
var createSlotWait = 30 * time.Second

// acquireCreateSlot returns a release func, or false when no slot freed up
// within createSlotWait. The channel is captured locally so a concurrent
// SetMaxConcurrent cannot make the release drain the wrong channel.
func (m *Manager) acquireCreateSlot() (func(), bool) {
	m.mu.RLock()
	lim := m.createLimiter
	m.mu.RUnlock()
	t := time.NewTimer(createSlotWait)
	defer t.Stop()
	select {
	case lim <- struct{}{}:
		return func() { <-lim }, true
	case <-t.C:
		return nil, false
	}
}
