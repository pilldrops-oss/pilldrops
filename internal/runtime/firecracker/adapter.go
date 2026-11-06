// Package firecracker adapts vm.Manager to the runtime.Runtime interface. It
// depends only on the vmBackend interface so the mapping is testable off-Linux.
package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/vm"
)

type vmBackend interface {
	Create(ctx context.Context, req vm.CreateRequest) (*vm.Sandbox, error)
	Exec(ctx context.Context, id, cmd, cwd string, timeout time.Duration) (*envd.ExecResult, error)
	ExecStream(ctx context.Context, id, cmd, cwd string, timeout time.Duration, onFrame func(envd.StreamFrame)) error
	ExecDetached(ctx context.Context, id, cmd, cwd string, timeout time.Duration) (*envd.ExecResult, error)
	ReadFile(ctx context.Context, id, path string) ([]byte, error)
	WriteFile(ctx context.Context, id, path string, data []byte) error
	Destroy(ctx context.Context, id string) error
	// DeliverIdentity pushes a booted sandbox's own credential into its guest.
	DeliverIdentity(id string, ident rt.Identity) error
	// Alive reports whether the VMM process is still running.
	Alive(id string) bool
	// SnapshotSandbox pauses the VM, writes a memory+state snapshot to destDir
	// and removes the sandbox from the active set, so a later Destroy is a no-op.
	SnapshotSandbox(ctx context.Context, id, destDir string) error
	// RestoreSandbox boots a VM from a per-sandbox snapshot in srcDir.
	RestoreSandbox(ctx context.Context, id, srcDir string, vcpu, memMB int) (*vm.Sandbox, error)
	// DiscardSnapshot releases the overlay, tap and records a snapshot in srcDir kept.
	DiscardSnapshot(srcDir string) error
	// SetNetworks replaces a sandbox's tag-network membership and returns the
	// address peers reach it on.
	SetNetworks(id string, n rt.Networks) (string, error)
	ForkSandbox(ctx context.Context, parentID, childID string, vcpu, memMB int) (*vm.Sandbox, time.Duration, error)
	ForkFromSnapshot(ctx context.Context, srcDir, childID string, vcpu, memMB int) (*vm.Sandbox, error)
	// BootFromCapture boots a new sandbox out of a capture fetched from
	// elsewhere, under the create's own egress policy.
	BootFromCapture(ctx context.Context, srcDir, childID string, vcpu, memMB int, eg *egress.Rules) (*vm.Sandbox, error)
	// CheckpointSandbox writes a named save point into dir; the sandbox keeps running.
	CheckpointSandbox(ctx context.Context, id, dir string) (time.Duration, int64, error)
	// RestoreCheckpoint rolls the sandbox back to the checkpoint in ckptDir in place.
	RestoreCheckpoint(ctx context.Context, id, ckptDir string, vcpu, memMB int) (*vm.Sandbox, error)
	// FreezeVM and ThawVM stop and start a VM's vCPUs, keeping it resident.
	FreezeVM(ctx context.Context, id string) error
	ThawVM(ctx context.Context, id string) error
	// RecordOwner, AdoptOwned and KillUnadopted are crash recovery: which
	// daemon sandbox a VM belongs to, reattaching it after a daemon restart,
	// and removing the VMs no sandbox claimed (vm/adopt.go).
	RecordOwner(vmID, owner string) error
	AdoptOwned(owner string) (*vm.Sandbox, bool)
	KillUnadopted(known []string)
}

// detachLog receives a detached exec's output; inheriting envd's fds would hang the RPC.
const detachLog = "/tmp/pd-detach.log"

const (
	defaultExecTimeout = 30 * time.Second
	// fcAppPort is injected as $PORT and matches docker's appPort so deploys are
	// portable across runtimes. Each VM has its own IP, so no collision.
	fcAppPort = 8000
	// fcTerminalPort is envd's web-terminal port inside the VM.
	fcTerminalPort = rt.TerminalPort
	// fcHomeDir must match envd's homeDir; relative FS paths and rt.AppLogPath resolve under it.
	fcHomeDir = "/root"
)

type fcRuntime struct {
	be vmBackend
	// netns mirrors vm.Config.NetNS; the isolation preflight differs per mode
	// and probing the wrong one would fail a healthy node.
	netns bool
	// warmRestoreStorageMB is the one writable-disk size golden restore serves
	// on this node, 0 when snapshots are off.
	warmRestoreStorageMB int
}

func newWithBackend(be vmBackend) *fcRuntime { return &fcRuntime{be: be} }

// NetNSMode reports whether this node boots sandboxes in per-sandbox network namespaces.
func (r *fcRuntime) NetNSMode() bool { return r.netns }

func (r *fcRuntime) Name() string { return "firecracker" }

// fcSnapshotsEnabled gates memory-snapshot pause/resume, and with it fork and
// checkpoints. ON unless turned off: set FC_MEM_SNAPSHOTS=0 (or false/no/off)
// to refuse them on a node. It was opt-in when the incident below was written.
//
// The commit turning this on by default is written and reverted, not abandoned.
// It was justified by 15
// pause/resume cycles at a 90 s gap with no failures, on a side-car with its own
// FC_NETNS_SLOT_BASE and a short FC_JAILER_DIR, which were believed to explain
// the intermittent "credential not accepted by the resumed guest" failure. That
// explanation did not hold: the failure then reproduced WITH both of those in
// place. The guest kernel acks the vsock CONNECT, so the device is back and
// what is missing is envd's accept loop — most likely a listening socket that
// does not survive the snapshot, which is also why a retry failed identically
// rather than transiently. Being on by default puts every Firecracker node on
// that path, driven by the hibernate timer rather than by anyone opting in, so
// FC_MEM_SNAPSHOTS=0 is the switch for a node that hits it.
func fcSnapshotsEnabled() bool { return config.Bool("FC_MEM_SNAPSHOTS", true) }

// errSnapshotsOff refuses everything that rides a memory capture: pause/resume,
// fork, checkpoints and named snapshots. One error, because one flag turns all
// of them off.
var errSnapshotsOff = errors.New("firecracker memory snapshots not enabled (set FC_MEM_SNAPSHOTS=1)")

// Capabilities is DeclaredCaps plus this node's WarmRestoreStorageMB.
func (r *fcRuntime) Capabilities() rt.Caps {
	caps := DeclaredCaps()
	caps.WarmRestoreStorageMB = r.warmRestoreStorageMB
	// Only the per-sandbox namespace has a chain to hold the policy; on the
	// shared bridge the host chains see only the post-SNAT source.
	caps.EgressPolicy = r.netns
	// Same condition, and nothing more: the name layer is a listener inside the
	// same namespace, started only for a rule set that names a domain, so there
	// is no flag to set and nothing to configure. A build without it reports
	// the field absent, which reads as false everywhere.
	caps.EgressDomains = r.netns
	// Same condition: the partition is a rule in the host's netns table, and
	// the shared bridge has no slot addresses to pair.
	caps.TagNetworks = r.netns
	// Stamping a new machine out of a capture reuses the baked tap name and
	// guest address, which only a private namespace has room for: on the shared
	// bridge the source's address is already live (vm.ErrForkNeedsNetNS).
	caps.NamedSnapshot = fcSnapshotsEnabled() && r.netns
	return caps
}

// DeclaredCaps is this runtime's capability declaration, readable without a
// constructed Runtime (a Firecracker Runtime needs KVM). Snapshot and
// Checkpoint both ride FC_MEM_SNAPSHOTS: a checkpoint is the same memory
// capture pointed at a named directory.
func DeclaredCaps() rt.Caps {
	return rt.Caps{
		Isolation: true, Snapshot: fcSnapshotsEnabled(),
		Checkpoint: fcSnapshotsEnabled(), MultiPort: true, L4Forward: true,
		// A pause writes RAM; the resume restores it with the app still
		// running, so the daemon must not start a second one.
		MemoryRestore: fcSnapshotsEnabled(),
		// Standby needs no flag: freezing the vCPUs is not a snapshot.
		Standby: true,
		// The guest owns its kernel, so a daemon inside it is just a daemon.
		NestedDocker: true,
		// A guest kernel hosts an X server unconditionally.
		Desktop: true,
	}
}
func (r *fcRuntime) Prepare(context.Context, rt.Template) error { return nil }

// CheckIsolation probes the mode the node actually runs: a canary netns with
// the in-ns firewall, or a canary tap isolated on fcbr0. Satisfies
// nodeboot.IsolationChecker structurally because nodeboot imports this package.
func (r *fcRuntime) CheckIsolation(ctx context.Context) error {
	if !vm.SeccompEnabled() {
		return errors.New("firecracker seccomp is not enabled (C2a regression)")
	}
	if r.netns {
		if err := vm.PreflightNetNS(); err != nil {
			return fmt.Errorf("netns containment: %w", err)
		}
		return nil
	}
	if err := vm.PreflightIsolation(); err != nil {
		return fmt.Errorf("tap isolation: %w", err)
	}
	return nil
}

func (r *fcRuntime) Boot(ctx context.Context, spec rt.BootSpec) (rt.Instance, error) {
	sb, err := r.be.Create(ctx, vm.CreateRequest{
		Template: spec.Template.Name,
		UserID:   "sandboxd",
		// The daemon owns TTL; a vm-layer backstop would destroy long-lived sandboxes behind its back.
		Timeout:   0,
		VCPUs:     spec.VCPU,
		MemoryMB:  spec.MemMiB,
		StorageMB: spec.StorageMB,
		Egress:    spec.Egress,
	})
	if err != nil {
		return nil, err
	}
	if err := r.deliverIdentity(ctx, sb.ID, spec); err != nil {
		return nil, err
	}
	return r.instance(ctx, sb, spec)
}

// instance wraps a booted VM and pushes the create env into the guest before
// anything can run there. A guest that cannot take its env is destroyed rather
// than handed out half-configured. The identity is delivered by the caller
// first, so a guest never sees its env without the credential that names it.
func (r *fcRuntime) instance(ctx context.Context, sb *vm.Sandbox, spec rt.BootSpec) (*fcInstance, error) {
	// Before anything else can fail: a daemon that crashes from here on must
	// find out which of its sandboxes this VM is.
	if err := r.be.RecordOwner(sb.ID, spec.SandboxID); err != nil {
		slog.Warn("could not record which sandbox a VM belongs to; it will not survive a daemon crash",
			"component", "firecracker", "vm_id", sb.ID, "sandbox_id", spec.SandboxID, "err", err)
	}
	i := &fcInstance{
		be: r.be, sandboxID: spec.SandboxID, vmID: sb.ID, hostIP: sb.HostIP(),
		vcpu: spec.VCPU, memMiB: spec.MemMiB, start: nowFunc(),
		envd:     guestClient(sb),
		token:    sb.Token,
		bootType: sb.BootType,
	}
	if len(spec.Template.Env) > 0 && i.envd != nil {
		if err := i.envd.SetEnv(ctx, envMap(spec.Template.Env)); err != nil {
			_ = r.be.Destroy(ctx, sb.ID)
			return nil, fmt.Errorf("push create env into the guest: %w", err)
		}
	}
	return i, nil
}

// envMap turns create-style "K=V" entries into a map; a later duplicate wins.
func envMap(kv []string) map[string]string {
	out := make(map[string]string, len(kv))
	for _, e := range kv {
		k, v, _ := strings.Cut(e, "=")
		out[k] = v
	}
	return out
}

// deliverIdentity hands the guest its own credential, or tears the VM down:
// a sandbox is never returned without the identity its row promises. A zero
// Identity (resume) delivers nothing.
func (r *fcRuntime) deliverIdentity(ctx context.Context, vmID string, spec rt.BootSpec) error {
	if spec.Identity.Token == "" {
		return nil
	}
	if err := r.be.DeliverIdentity(vmID, spec.Identity); err != nil {
		_ = r.be.Destroy(ctx, vmID)
		return fmt.Errorf("sandbox %s: identity push failed, VM torn down: %w", spec.SandboxID, err)
	}
	return nil
}

func (r *fcRuntime) Restore(ctx context.Context, ref rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	if !fcSnapshotsEnabled() {
		return nil, errSnapshotsOff
	}
	sb, err := r.be.RestoreSandbox(ctx, spec.SandboxID, ref.Path, spec.VCPU, spec.MemMiB)
	if err != nil {
		return nil, err
	}
	return r.instance(ctx, sb, spec)
}

// Fork copies a parent into spec.SandboxID. A running parent is paused only
// while its memory is written and its disk reflinked; a paused one is copied
// from its snapshot and stays paused. Same gate as pause/resume.
func (r *fcRuntime) Fork(ctx context.Context, src rt.ForkSource, spec rt.BootSpec) (rt.Instance, rt.ForkInfo, error) {
	if !fcSnapshotsEnabled() {
		return nil, rt.ForkInfo{}, errSnapshotsOff
	}
	var (
		sb     *vm.Sandbox
		paused time.Duration
		err    error
	)
	if src.Parent != nil {
		parent, ok := src.Parent.(*fcInstance)
		if !ok {
			return nil, rt.ForkInfo{}, fmt.Errorf("fork: parent is not a firecracker instance")
		}
		sb, paused, err = r.be.ForkSandbox(ctx, parent.vmID, spec.SandboxID, spec.VCPU, spec.MemMiB)
	} else {
		sb, err = r.be.ForkFromSnapshot(ctx, src.Snapshot.Path, spec.SandboxID, spec.VCPU, spec.MemMiB)
	}
	if err != nil {
		return nil, rt.ForkInfo{}, err
	}
	// A fork is a new sandbox: it gets its own identity, never the parent's.
	if err := r.deliverIdentity(ctx, sb.ID, spec); err != nil {
		return nil, rt.ForkInfo{}, err
	}
	// The child inherits the parent's /etc/template.env; re-sending the
	// daemon's registered env is an idempotent merge.
	inst, err := r.instance(ctx, sb, spec)
	if err != nil {
		return nil, rt.ForkInfo{}, err
	}
	return inst, rt.ForkInfo{ParentPaused: paused}, nil
}

// Checkpoint implements rt.Checkpointer: a named memory+disk save point, taken
// with the guest stopped only for the writes. Same gate as pause/resume and fork.
func (r *fcRuntime) Checkpoint(ctx context.Context, inst rt.Instance, dst rt.SnapshotRef) (rt.CheckpointInfo, error) {
	if !fcSnapshotsEnabled() {
		return rt.CheckpointInfo{}, errSnapshotsOff
	}
	i, ok := inst.(*fcInstance)
	if !ok {
		return rt.CheckpointInfo{}, fmt.Errorf("checkpoint: not a firecracker instance")
	}
	paused, size, err := r.be.CheckpointSandbox(ctx, i.vmID, dst.Path)
	if err != nil {
		return rt.CheckpointInfo{}, err
	}
	return rt.CheckpointInfo{Paused: paused, Kind: "memory", SizeBytes: size}, nil
}

// RestoreCheckpoint rolls the sandbox back in place: same VM id, so its overlay,
// lineage and records keep the key they were registered under. The instance is
// replaced because a fresh namespace means a fresh host address.
func (r *fcRuntime) RestoreCheckpoint(ctx context.Context, inst rt.Instance, src rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	if !fcSnapshotsEnabled() {
		return nil, errSnapshotsOff
	}
	i, ok := inst.(*fcInstance)
	if !ok {
		return nil, fmt.Errorf("restore checkpoint: not a firecracker instance")
	}
	sb, err := r.be.RestoreCheckpoint(ctx, i.vmID, src.Path, spec.VCPU, spec.MemMiB)
	if err != nil {
		return nil, err
	}
	return r.instance(ctx, sb, spec)
}

var _ rt.Checkpointer = (*fcRuntime)(nil)

// DiskPressured implements rt.DiskPressured: the watchdog's flag, readable by
// the create path before it fetches anything. A backend that does not track it
// reports false, which is the answer every non-Firecracker runtime gives.
func (r *fcRuntime) DiskPressured() bool {
	p, ok := r.be.(interface{ DiskPressure() bool })
	return ok && p.DiskPressure()
}

var _ rt.DiskPressured = (*fcRuntime)(nil)

// BootFromSnapshot stamps a new sandbox out of a published capture. The same
// restore a fork uses, so the machine wakes with its processes running and its
// listeners up; what differs is that the capture came from the artifact store
// rather than from a parent on this node, and the child takes the create's own
// egress policy rather than the captured sandbox's.
func (r *fcRuntime) BootFromSnapshot(ctx context.Context, src rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, string, error) {
	if !fcSnapshotsEnabled() {
		return nil, "", errSnapshotsOff
	}
	if !r.netns {
		// Same reason a fork needs them: the capture's tap name and guest
		// address are baked into the memory image and are already live on the
		// shared bridge.
		return nil, "", vm.ErrForkNeedsNetNS
	}
	sb, err := r.be.BootFromCapture(ctx, src.Path, spec.SandboxID, spec.VCPU, spec.MemMiB, spec.Egress)
	if err != nil {
		return nil, "", err
	}
	// A new sandbox, so a new identity: the memory image still holds the
	// captured machine's credential.
	if err := r.deliverIdentity(ctx, sb.ID, spec); err != nil {
		return nil, "", err
	}
	inst, err := r.instance(ctx, sb, spec)
	if err != nil {
		return nil, "", err
	}
	return inst, "memory", nil
}

var _ rt.SnapshotBooter = (*fcRuntime)(nil)

func (r *fcRuntime) RecoverOrphans() []rt.Instance { return nil }

// DeleteSnapshot implements rt.SnapshotDeleter: a paused-then-destroyed sandbox
// releases the overlay, lineage, tap and records its pause snapshot kept. The
// snapshot dir itself is the daemon's to remove.
func (r *fcRuntime) DeleteSnapshot(_ context.Context, _ string, snap rt.SnapshotRef) error {
	if snap.Path == "" {
		return nil
	}
	return r.be.DiscardSnapshot(snap.Path)
}

var _ rt.SnapshotDeleter = (*fcRuntime)(nil)

var nowFunc = time.Now

type fcInstance struct {
	be        vmBackend
	sandboxID string // the daemon's id
	vmID      string // the vm layer's id, used for backend calls
	// hostIP is the address THIS NODE can reach the guest on: under FC_NETNS the
	// sandbox's namespace slot address, on the shared bridge the guest's own IP.
	// Never the in-namespace guest IP, which is the same in every sandbox.
	hostIP string
	vcpu   int
	memMiB int
	start  time.Time
	// envd is this VM's guest agent, for the calls the vmBackend does not
	// carry (SetEnv, streamed files, stdin/raw exec). nil in tests with a
	// fake backend, which then take the buffered path.
	envd *envd.Client
	// token is the per-sandbox envd credential, empty when EnvdAuth is off.
	token string
	// bootType is what the VM layer did: "snapshot" (golden restore) or "cold".
	bootType string
}

// BootType implements rt.BootTyper.
func (i *fcInstance) BootType() string { return i.bootType }

// SetNetworks implements rt.Networker. It is on the instance because only the
// instance knows this sandbox's vm-layer id, which is not the daemon's.
func (i *fcInstance) SetNetworks(_ context.Context, n rt.Networks) (string, error) {
	return i.be.SetNetworks(i.vmID, n)
}

// Freeze implements rt.Freezer: stop the vCPUs, keep the RAM. Unlike Snapshot
// it needs no FC_MEM_SNAPSHOTS, because nothing is written out.
func (i *fcInstance) Freeze(ctx context.Context) error { return i.be.FreezeVM(ctx, i.vmID) }

// Thaw implements rt.Freezer.
func (i *fcInstance) Thaw(ctx context.Context) error { return i.be.ThawVM(ctx, i.vmID) }

var _ rt.Freezer = (*fcInstance)(nil)

// EnvdProtocol implements rt.EnvdProtocoler: the level the template's baked
// envd reported at connect.
func (i *fcInstance) EnvdProtocol() int {
	if i.envd == nil {
		return 1
	}
	return i.envd.Protocol()
}

// SetEnv implements rt.EnvSetter: a /config change reaches shells, not only
// the next exec.
func (i *fcInstance) SetEnv(ctx context.Context, env map[string]string) error {
	if i.envd == nil {
		return rt.ErrUnsupported
	}
	return i.envd.SetEnv(ctx, env)
}

func (i *fcInstance) ID() string { return i.sandboxID }

// Alive reports whether this VM's VMM process is still running.
func (i *fcInstance) Alive() bool { return i.be.Alive(i.vmID) }

// OOMKilled implements rt.OOMKilledReporter: the host cgroup killed the VMM.
func (i *fcInstance) OOMKilled() bool {
	o, ok := i.be.(interface{ OOMKilled(string) bool })
	return ok && o.OOMKilled(i.vmID)
}

func (i *fcInstance) Exec(ctx context.Context, s rt.ExecSpec) (rt.ExecResult, error) {
	to := s.Timeout
	if to == 0 {
		to = defaultExecTimeout
	}
	full := withEnv(execEnv(s.Env), s.Cmd)
	if len(s.Stdin) > 0 && i.envd != nil && !s.Detach {
		// ponytail: stdin rides the envd client directly; the vmBackend carries no stdin.
		res, err := i.envd.Run(ctx, envd.ExecOpts{Cmd: full, Cwd: s.Cwd, Timeout: to, Stdin: s.Stdin})
		if err != nil {
			return rt.ExecResult{}, err
		}
		return rt.ExecResult{Stdout: string(res.Stdout), Stderr: string(res.Stderr), ExitCode: res.ExitCode, Truncated: res.Truncated}, nil
	}
	if s.Detach {
		// Redirect away from envd's fds or the RPC hangs; echo the pid as the handle.
		wrapped := "{ " + full + " ; } >" + detachLog + " 2>&1 & echo $!"
		res, err := i.be.ExecDetached(ctx, i.vmID, wrapped, s.Cwd, 10*time.Second)
		if err != nil {
			return rt.ExecResult{}, err
		}
		pid := 0
		if res != nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
		}
		return rt.ExecResult{Pid: pid}, nil
	}
	res, err := i.be.Exec(ctx, i.vmID, full, s.Cwd, to)
	if err != nil {
		return rt.ExecResult{}, err
	}
	return rt.ExecResult{
		Stdout: string(res.Stdout), Stderr: string(res.Stderr),
		ExitCode: res.ExitCode, Truncated: res.Truncated,
	}, nil
}

// ExecStream implements rt.StreamExecutor. A detached spec is not streamable
// and falls back to a single terminal frame.
func (i *fcInstance) ExecStream(ctx context.Context, s rt.ExecSpec, onFrame func(rt.StreamFrame)) error {
	if s.Detach {
		res, err := i.Exec(ctx, s)
		if err != nil {
			return err
		}
		onFrame(rt.StreamFrame{Exited: true, ExitCode: res.ExitCode})
		return nil
	}
	to := s.Timeout
	if to == 0 {
		to = defaultExecTimeout
	}
	full := withEnv(execEnv(s.Env), s.Cmd)
	relay := func(f envd.StreamFrame) {
		onFrame(rt.StreamFrame{
			Stdout: f.Stdout, Stderr: f.Stderr,
			ExitCode: f.ExitCode, Exited: f.Exited, Truncated: f.Truncated,
		})
	}
	if (len(s.Stdin) > 0 || s.RawOutput) && i.envd != nil {
		return i.envd.RunStream(ctx, envd.ExecOpts{Cmd: full, Cwd: s.Cwd, Timeout: to, Stdin: s.Stdin, NoOutputCap: s.RawOutput}, relay)
	}
	return i.be.ExecStream(ctx, i.vmID, full, s.Cwd, to, relay)
}

func (i *fcInstance) FS() rt.FileSystem { return &fcFS{be: i.be, id: i.vmID, envd: i.envd} }

// Procs lists guest processes via ps. Best-effort: a failure returns an empty list, not an error.
func (i *fcInstance) Procs() ([]rt.Proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultExecTimeout)
	defer cancel()
	res, err := i.be.Exec(ctx, i.vmID, "ps -eo pid,comm --no-headers 2>/dev/null", "", defaultExecTimeout)
	if err != nil || res == nil || res.ExitCode != 0 {
		return nil, nil
	}
	var procs []rt.Proc
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if pid, perr := strconv.Atoi(f[0]); perr == nil {
			procs = append(procs, rt.Proc{PID: pid, Cmd: strings.Join(f[1:], " ")})
		}
	}
	return procs, nil
}

// OOMKillCount counts OOM kills from the guest's own dmesg, which is sandbox-private.
func (i *fcInstance) OOMKillCount(ctx context.Context) (int, error) {
	res, err := i.be.Exec(ctx, i.vmID,
		`dmesg 2>/dev/null | grep -c "Out of memory: Killed process"`, "", defaultExecTimeout)
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	// grep -c exits 1 with no matches, so ignore ExitCode and parse the number.
	n, _ := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	return n, nil
}

// Signal sends a signal to a guest process via envd.
func (i *fcInstance) Signal(pid, sig int) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultExecTimeout)
	defer cancel()
	res, err := i.be.Exec(ctx, i.vmID,
		fmt.Sprintf("kill -%d %d", sig, pid), "", defaultExecTimeout)
	if err != nil {
		return err
	}
	if res != nil && res.ExitCode != 0 {
		return fmt.Errorf("kill -%d %d: %s", sig, pid, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// ListenTarget returns hostIP:port; port 0 falls back to fcAppPort.
func (i *fcInstance) ListenTarget(port int) (string, error) {
	if i.hostIP == "" {
		return "", errors.New("sandbox has no VM IP")
	}
	if port <= 0 {
		port = fcAppPort
	}
	return i.hostIP + ":" + strconv.Itoa(port), nil
}

// TerminalTarget returns hostIP:fcTerminalPort.
func (i *fcInstance) TerminalTarget() (string, error) {
	if i.hostIP == "" {
		return "", errors.New("sandbox has no VM IP")
	}
	return i.hostIP + ":" + strconv.Itoa(fcTerminalPort), nil
}

// TerminalToken returns the per-sandbox envd credential.
func (i *fcInstance) TerminalToken() (string, error) {
	return i.token, nil
}

// StartServer backgrounds the command with $PORT injected via the detached exec
// path; a plain exec would group-kill the server when the RPC returns. The
// create env comes from /etc/template.env through envd's session env.
func (i *fcInstance) StartServer(ctx context.Context, cmd string, env map[string]string) error {
	// Log under the guest home, where the /logs endpoint reads rt.AppLogPath. Append so restarts accrete.
	logPath := path.Join(fcHomeDir, rt.AppLogPath)
	wrapped := fmt.Sprintf("mkdir -p %s; PORT=%d sh -c %s >> %s 2>&1 &",
		shellQuote(path.Dir(logPath)), fcAppPort, shellQuote(withEnv(env, cmd)), shellQuote(logPath))
	_, err := i.be.ExecDetached(ctx, i.vmID, wrapped, "", 10*time.Second)
	return err
}

// Snapshot pauses the VM and writes a memory+state snapshot to ref.Path.
func (i *fcInstance) Snapshot(ctx context.Context, ref rt.SnapshotRef) error {
	if !fcSnapshotsEnabled() {
		return errSnapshotsOff
	}
	return i.be.SnapshotSandbox(ctx, i.vmID, ref.Path)
}

func (i *fcInstance) Stop(ctx context.Context) error { return i.be.Destroy(ctx, i.vmID) }

func (i *fcInstance) Metrics() rt.InstanceMetrics {
	return rt.InstanceMetrics{VCPU: i.vcpu, MemMiB: i.memMiB, UptimeSecs: time.Since(i.start).Seconds()}
}

// execEnv is the exec env with $PORT parity with the start command; an explicit
// request PORT wins.
func execEnv(in map[string]string) map[string]string {
	if _, ok := in["PORT"]; ok {
		return in
	}
	env := make(map[string]string, len(in)+1)
	for k, v := range in {
		env[k] = v
	}
	env["PORT"] = strconv.Itoa(fcAppPort)
	return env
}

// withEnv prefixes cmd with `env K=V ...` for the per-call env only; the
// create env lives in the guest's /etc/template.env (SetEnv) so no secret
// rides a command line where /proc/*/cmdline would show it.
func withEnv(env map[string]string, cmd string) string {
	if len(env) == 0 {
		return cmd
	}
	var b strings.Builder
	b.WriteString("env ")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(shellQuote(k + "=" + env[k]))
		b.WriteByte(' ')
	}
	// A child shell so $VAR in cmd expands against the injected vars; the
	// outer shell has already expanded them before env runs.
	b.WriteString("sh -c ")
	b.WriteString(shellQuote(cmd))
	return b.String()
}

// fcFS implements runtime.FileSystem by running shell commands in the VM via envd.
type fcFS struct {
	be   vmBackend
	id   string
	envd *envd.Client // nil with a fake backend: StreamFS then buffers through be
}

// WriteFrom implements rt.StreamFS over envd's chunked upload.
func (f *fcFS) WriteFrom(ctx context.Context, p string, r io.Reader) (int64, error) {
	if f.envd == nil {
		data, err := io.ReadAll(r)
		if err != nil {
			return 0, err
		}
		return int64(len(data)), pathRefusal(p, f.be.WriteFile(ctx, f.id, p, data))
	}
	n, err := f.envd.WriteFrom(ctx, p, r)
	return n, pathRefusal(p, err)
}

// ReadTo implements rt.StreamFS over envd's chunked download.
func (f *fcFS) ReadTo(ctx context.Context, p string, w io.Writer) error {
	if f.envd == nil {
		data, err := f.be.ReadFile(ctx, f.id, p)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	return f.envd.ReadTo(ctx, p, w)
}

// confineShell prefixes a guest-side realpath guard that exits before the real
// op if p resolves outside root. It runs in the guest, where symlinks are
// meaningful, and relies on envd defaulting an empty cwd to root so a relative
// p resolves correctly.
func confineShell(root, p string) string {
	// `||` rather than `&&`: a realpath failure must exit, not short-circuit
	// past the case block into the unguarded op.
	return fmt.Sprintf(
		`p=$(realpath -m -- %s) || { echo "path resolution failed" >&2; exit 1; }; case "$p" in %s|%s/*) ;; *) echo "path escapes workspace" >&2; exit 1;; esac; `,
		shellQuote(p), shellQuote(root), shellQuote(root))
}

// run executes a helper command in the guest against path p.
func (f *fcFS) run(ctx context.Context, p, cmd string) (string, error) {
	res, err := f.be.Exec(ctx, f.id, cmd, "", defaultExecTimeout)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fsError(p, string(res.Stderr), res.ExitCode)
	}
	return string(res.Stdout), nil
}

// fsError turns a guest helper's failure into one the API can classify and the
// caller can act on. These helpers are find(1), stat(1), mkdir(1), mv(1) and
// rm(1) — an implementation detail of this file, and not something a person who
// asked for a path should have to read. A missing path is the common case and
// the one worth naming, because it was reported as a 404 with the raw guest
// stderr and the CLI then told the caller to go and check the sandbox id, which
// had never been wrong.
func fsError(p, stderr string, code int) error {
	msg := strings.TrimSpace(stderr)
	if strings.Contains(msg, "No such file or directory") {
		return fmt.Errorf("%w: %s", rt.ErrPathNotFound, p)
	}
	// confineShell's own refusals: the caller's path, every time.
	if strings.Contains(msg, "path escapes workspace") || strings.Contains(msg, "path resolution failed") {
		return fmt.Errorf("%w: %s", rt.ErrPathRefused, p)
	}
	// Drop the leading "tool: " the helper stamps on anything else it says.
	if tool, rest, ok := strings.Cut(msg, ": "); ok && !strings.ContainsAny(tool, " \t") {
		msg = rest
	}
	if msg == "" {
		msg = fmt.Sprintf("the sandbox refused it (exit %d)", code)
	}
	if kind := rt.FSKind(msg); kind != nil {
		return fmt.Errorf("%w: %s: %s", kind, p, msg)
	}
	return fmt.Errorf("%s: %s", p, msg)
}

// pathRefusal tags the guest's refusal of a path, so the API can answer the
// caller rather than blame the node. envd confines every file path to the
// sandbox home and answers one deliberately generic "invalid path" for anything
// outside it — kept generic on purpose, so an escape attempt and a typo cannot
// be told apart from outside.
//
// ponytail: matched on the message. envd returns it as an ordinary error and
// gRPC flattens that to codes.Unknown; give the RPC a codes.InvalidArgument and
// check the code here when envd is next changed.
func pathRefusal(p string, err error) error {
	if err != nil && strings.Contains(err.Error(), "invalid path") {
		return fmt.Errorf("%w: %s", rt.ErrPathRefused, p)
	}
	if err != nil {
		if kind := rt.FSKind(err.Error()); kind != nil {
			return fmt.Errorf("%w: %s: %w", kind, p, err)
		}
	}
	return err
}

func (f *fcFS) Read(ctx context.Context, p string) ([]byte, error) {
	data, err := f.be.ReadFile(ctx, f.id, p)
	return data, pathRefusal(p, err)
}
func (f *fcFS) Write(ctx context.Context, p string, data []byte) error {
	return pathRefusal(p, f.be.WriteFile(ctx, f.id, p, data))
}

// listCmd lists p with NUL-delimited records because a filename may contain
// tabs or newlines. find lists nothing for a file, so a file is refused as on
// docker rather than answered as an empty directory.
func listCmd(root, p string) string {
	return confineShell(root, p) +
		`[ -e "$p" ] || { echo "No such file or directory" >&2; exit 1; }; ` +
		`[ -d "$p" ] || { echo "Not a directory" >&2; exit 1; }; ` +
		"find " + shellQuote(p) + ` -maxdepth 1 -mindepth 1 -printf '%y\t%s\t%f\0'`
}

// removeCmd removes p, but never the sandbox home itself.
func removeCmd(root, p string) string {
	return confineShell(root, p) +
		`[ "$p" = ` + shellQuote(root) + ` ] && { echo "refusing to remove the sandbox home" >&2; exit 1; }; ` +
		"rm -rf " + shellQuote(p)
}

func (f *fcFS) List(ctx context.Context, p string) ([]rt.DirEntry, error) {
	out, err := f.run(ctx, p, listCmd(fcHomeDir, p))
	if err != nil {
		return nil, err
	}
	var entries []rt.DirEntry
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3) // type, size, name
		if len(parts) < 3 {
			continue
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		entries = append(entries, rt.DirEntry{Name: parts[2], IsDir: parts[0] == "d", Size: size})
	}
	return entries, nil
}

func (f *fcFS) Mkdir(ctx context.Context, p string) error {
	_, err := f.run(ctx, p, confineShell(fcHomeDir, p)+"mkdir -p "+shellQuote(p))
	return err
}
func (f *fcFS) Move(ctx context.Context, src, dst string) error {
	// Guard both endpoints or a confined src could be moved out of the workspace.
	_, err := f.run(ctx, src, confineShell(fcHomeDir, src)+confineShell(fcHomeDir, dst)+
		"mv "+shellQuote(src)+" "+shellQuote(dst))
	return err
}
func (f *fcFS) Remove(ctx context.Context, p string) error {
	_, err := f.run(ctx, p, removeCmd(fcHomeDir, p))
	return err
}

// Stat parses `stat -c '%F\t%s\t%A'` (type, size, mode); none of those fields can contain a tab.
func (f *fcFS) Stat(ctx context.Context, p string) (rt.FileInfo, error) {
	out, err := f.run(ctx, p, confineShell(fcHomeDir, p)+"stat -c '%F\t%s\t%A' "+shellQuote(p))
	if err != nil {
		return rt.FileInfo{}, err
	}
	parts := strings.SplitN(strings.TrimRight(out, "\n"), "\t", 3)
	if len(parts) < 3 {
		return rt.FileInfo{}, fmt.Errorf("unexpected stat output: %q", out)
	}
	size, _ := strconv.ParseInt(parts[1], 10, 64)
	return rt.FileInfo{
		Name:  path.Base(p),
		IsDir: strings.Contains(parts[0], "directory"),
		Size:  size,
		Mode:  parts[2],
	}, nil
}

// shellQuote single-quotes s for safe inclusion in a /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Adopt implements rt.Adopter: reattach the VM a crashed daemon left running
// for sandboxID. The daemon's restore calls it for every sandbox its records
// say was running.
func (r *fcRuntime) Adopt(_ context.Context, sandboxID string) (rt.Instance, error) {
	sb, ok := r.be.AdoptOwned(sandboxID)
	if !ok {
		return nil, rt.ErrNotFound
	}
	return &fcInstance{
		be: r.be, sandboxID: sandboxID, vmID: sb.ID, hostIP: sb.HostIP(),
		start: nowFunc(), envd: guestClient(sb), token: sb.Token, bootType: sb.BootType,
	}, nil
}

// SweepUnknown implements rt.Sweeper: once the daemon has adopted what it
// owns, every VM still held for adoption belongs to nothing and is removed.
func (r *fcRuntime) SweepUnknown(known []string) { r.be.KillUnadopted(known) }

var (
	_ rt.Adopter = (*fcRuntime)(nil)
	_ rt.Sweeper = (*fcRuntime)(nil)
)
