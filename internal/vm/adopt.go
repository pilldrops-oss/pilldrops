//go:build linux

package vm

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
)

// Crash recovery. A VMM outlives its daemon (no signal is forwarded to it), but
// the daemon names sandboxes by its own id and the VM layer by another, and
// nothing linked the two, so a restarted daemon could only kill every VM it
// found: a crash, an OOM kill or a panic lost every running sandbox on the node.
//
// Every VM a daemon sandbox runs on now has an adoption record naming its owner
// and what the VM layer needs to drive it again. At startup a VM with a record
// is held (recoverable) instead of killed; the daemon's restore adopts the ones
// its records say are running, and KillUnadopted removes the rest.

type adoptRecord struct {
	Owner       string        `json:"owner"`
	VMID        string        `json:"vm_id"`
	Template    string        `json:"template"`
	RootfsPath  string        `json:"rootfs_path,omitempty"`
	OverlayPath string        `json:"overlay_path"`
	TapName     string        `json:"tap_name,omitempty"`
	VMIP        string        `json:"vmip"`
	NodeAddr    string        `json:"node_addr"`
	NetNS       string        `json:"netns,omitempty"`
	Token       string        `json:"token,omitempty"`
	Egress      *egress.Rules `json:"egress,omitempty"`
	Volumes     []string      `json:"volumes,omitempty"`
}

func (m *Manager) adoptPath(vmID string) string {
	return filepath.Join(m.tokenDir(), vmID+".adopt")
}

// RecordOwner writes vmID's adoption record naming owner, the daemon's id for
// the sandbox. Called whenever a daemon sandbox gets a VM (boot, restore, fork).
func (m *Manager) RecordOwner(vmID, owner string) error {
	m.mu.RLock()
	sb, ok := m.active[vmID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	b, err := json.Marshal(adoptRecord{
		Owner: owner, VMID: vmID, Template: sb.Template, RootfsPath: sb.RootfsPath,
		OverlayPath: sb.OverlayPath, TapName: sb.TapName, VMIP: sb.VMIP, NodeAddr: sb.NodeAddr,
		NetNS: sb.NetNS, Token: sb.Token, Egress: sb.Egress, Volumes: sb.Volumes,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.tokenDir(), 0o700); err != nil {
		return err
	}
	tmp := m.adoptPath(vmID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { // holds the envd token
		return err
	}
	return os.Rename(tmp, m.adoptPath(vmID))
}

func (m *Manager) removeOwner(vmID string) { _ = os.Remove(m.adoptPath(vmID)) }

func (m *Manager) readAdopt(vmID string) (adoptRecord, bool) {
	var r adoptRecord
	b, err := os.ReadFile(m.adoptPath(vmID))
	if err != nil || json.Unmarshal(b, &r) != nil || r.Owner == "" {
		return r, false
	}
	return r, true
}

// AdoptOwned reattaches the running VM whose record names owner. false means
// there is none, or it no longer answers (it is then removed).
func (m *Manager) AdoptOwned(owner string) (*Sandbox, bool) {
	m.mu.Lock()
	var proc *OrphanProcess
	var rec adoptRecord
	for id, p := range m.recoverable {
		if r, ok := m.readAdopt(id); ok && r.Owner == owner {
			proc, rec = p, r
			delete(m.recoverable, id)
			break
		}
	}
	m.mu.Unlock()
	if proc == nil {
		return nil, false
	}

	// A VM whose memory was still being served lazily lost its server with
	// the daemon: the next touch of a page not yet fetched would freeze the
	// guest for good. Remove it rather than adopt a machine that will hang.
	if _, err := os.Stat(filepath.Join(jailerChrootDir(), rec.VMID, "root", lazyPendingMarker)); err == nil {
		m.log.Error("adopt_refused_lazy_memory", zap.String("vm_id", rec.VMID), zap.String("owner", owner))
		m.strandOrphan(proc, owner, rec.OverlayPath, "its memory was still being served on demand when the daemon stopped")
		return nil, false
	}

	root := filepath.Join(jailerChrootDir(), rec.VMID, "root")
	machine := &attachedMachine{
		pid:    proc.Machine.Pid(),
		root:   root,
		client: firecracker.NewClient(filepath.Join(root, "run", rec.VMID+".sock"), quietLogrus(), false),
	}
	// A daemon that stopped mid-capture left the guest frozen, and a frozen guest cannot answer envd.
	if err := unfreeze(context.Background(), machine); err != nil {
		m.log.Warn("adopt_thaw_failed", zap.String("vm_id", rec.VMID), zap.Error(err))
	}

	var client *envd.Client
	var err error
	if rec.Token == "" {
		client, err = envd.Connect(rec.NodeAddr, 10*time.Second)
	} else {
		client, err = envd.ConnectWithToken(rec.NodeAddr, rec.Token, 10*time.Second)
	}
	if err != nil {
		m.log.Warn("adopt_failed_envd", zap.String("vm_id", rec.VMID), zap.String("owner", owner), zap.Error(err))
		m.strandOrphan(proc, owner, rec.OverlayPath, "adoption: envd did not answer: "+err.Error())
		return nil, false
	}
	sb := &Sandbox{
		ID: rec.VMID, Template: rec.Template, RootfsPath: rec.RootfsPath, UserID: "sandboxd",
		Machine: machine,
		Envd:    client, OverlayPath: rec.OverlayPath, TapName: rec.TapName, VMIP: rec.VMIP,
		NodeAddr: rec.NodeAddr, NetNS: rec.NetNS, Token: rec.Token, Egress: rec.Egress, Volumes: rec.Volumes,
		BootType: "adopted", CreatedAt: time.Now(),
		// MemBase stays empty: without a lineage the next snapshot is a full one.
		stopTimer: func() {},
	}
	// The name layer ran in the dead daemon, and the namespace still diverts
	// 80/443 to it: without a new one every allowed domain is refused.
	if rec.NetNS != "" {
		if err := startNameLayer(rec.NetNS, netnsRunDir+"/"+rec.NetNS, rec.VMID, rec.Egress, m.log); err != nil {
			m.log.Error("adopt_name_layer_failed", zap.String("vm_id", rec.VMID), zap.Error(err))
		}
	}
	m.mu.Lock()
	m.active[rec.VMID] = sb
	m.mu.Unlock()
	m.log.Info("vm_adopted", zap.String("vm_id", rec.VMID), zap.String("owner", owner))
	return sb, true
}

// KillUnadopted removes every held VM the daemon did not claim, and every
// stranded disk whose sandbox is not among known.
func (m *Manager) KillUnadopted(known []string) {
	m.mu.Lock()
	left := m.recoverable
	m.recoverable = map[string]*OrphanProcess{}
	stranded := m.stranded
	m.stranded = map[string]string{}
	m.mu.Unlock()
	for owner, overlay := range stranded {
		if !slices.Contains(known, owner) {
			deleteOverlay(overlay)
		}
	}
	for id, p := range left {
		rec, _ := m.readAdopt(id)
		m.killOrphan(p, rec.OverlayPath, "no daemon sandbox claimed it after the restart")
	}
}

// strandOrphan stops a VM that could not be adopted and keeps its disk: the
// sandbox's record may hold a snapshot that resumes on it, though the disk is now
// newer than that snapshot. KillUnadopted removes the disk if no record claims it.
func (m *Manager) strandOrphan(proc *OrphanProcess, owner, overlay, reason string) {
	if !m.stopOrphan(proc, reason) {
		return
	}
	if overlay == "" {
		overlay = proc.OverlayPath
	}
	m.mu.Lock()
	m.stranded[owner] = overlay
	m.mu.Unlock()
}

// killOrphan stops an orphaned VMM and releases what it held, disk included.
func (m *Manager) killOrphan(proc *OrphanProcess, overlay, reason string) {
	if !m.stopOrphan(proc, reason) {
		return
	}
	if overlay == "" {
		overlay = proc.OverlayPath
	}
	deleteOverlay(overlay)
}

// stopOrphan stops an orphaned VMM and releases all but its disk. false means it
// is still running, so its backing files must stay.
func (m *Manager) stopOrphan(proc *OrphanProcess, reason string) bool {
	killErr := proc.Kill()
	m.log.Info("killing_orphan", zap.String("sandbox_id", proc.SandboxID),
		zap.String("reason", reason), zap.Error(killErr))
	if killErr != nil {
		// Never unlink the backing files of a VM that is still running.
		m.log.Warn("orphan_still_running_after_kill", zap.String("sandbox_id", proc.SandboxID), zap.Error(killErr))
		return false
	}
	cleanJailerRoot(filepath.Join(jailerChrootDir(), proc.SandboxID, "root"))
	if rec, ok := m.readAdopt(proc.SandboxID); ok && rec.NetNS != "" {
		teardownNetNS(rec.NetNS)
	}
	if tap := m.loadTap(proc.SandboxID); tap != "" {
		TeardownVMNetwork(tap)
	}
	m.removeToken(proc.SandboxID)
	m.removeEndpoint(proc.SandboxID)
	m.removeTap(proc.SandboxID)
	m.removeOwner(proc.SandboxID)
	return true
}

// attachedMachine drives a VMM this daemon did not start, through its API
// socket: the SDK's Machine can only drive one it launched.
type attachedMachine struct {
	pid    int
	root   string
	client *firecracker.Client
}

func (a *attachedMachine) PauseVM(ctx context.Context) error {
	_, err := a.client.PatchVM(ctx, &models.VM{State: firecracker.String(models.VMStatePaused)})
	return err
}

func (a *attachedMachine) ResumeVM(ctx context.Context) error {
	_, err := a.client.PatchVM(ctx, &models.VM{State: firecracker.String(models.VMStateResumed)})
	return err
}

func (a *attachedMachine) snapshot(ctx context.Context, statePath, memPath, kind string) error {
	_, err := a.client.CreateSnapshot(ctx, &models.SnapshotCreateParams{
		MemFilePath: firecracker.String(memPath), SnapshotPath: firecracker.String(statePath), SnapshotType: kind,
	})
	return err
}

func (a *attachedMachine) CreateSnapshot(ctx context.Context, statePath, memPath string) error {
	return a.snapshot(ctx, statePath, memPath, models.SnapshotCreateParamsSnapshotTypeFull)
}

func (a *attachedMachine) CreateDiffSnapshot(ctx context.Context, statePath, memPath string) error {
	return a.snapshot(ctx, statePath, memPath, models.SnapshotCreateParamsSnapshotTypeDiff)
}

func (a *attachedMachine) Stop(context.Context) error {
	return stopVMMBounded(a.pid, func() error { return syscall.Kill(a.pid, syscall.SIGTERM) })
}

func (a *attachedMachine) Pid() int           { return a.pid }
func (a *attachedMachine) ChrootRoot() string { return a.root }

func quietLogrus() *logrus.Entry {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return logrus.NewEntry(l)
}
