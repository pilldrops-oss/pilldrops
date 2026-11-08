//go:build !linux

// Compile-only stand-in for the Linux-only Firecracker VM manager so dependents
// build on non-Linux hosts. Every operation returns ErrUnsupported.
package vm

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// ErrUnsupported is returned by every stub operation.
var ErrUnsupported = errors.New("firecracker VM engine requires Linux+KVM")

// Config mirrors the Linux Config.
type Config struct {
	SnapshotDir     string
	OverlayDir      string
	FirecrackerBin  string
	JailerBin       string
	KernelPath      string
	Templates       []string
	MaxConcurrent   int
	MaxActive       int
	MemoryMB        int
	VCPUs           int
	Resources       map[string]runtime.Resources
	EnableSnapshots bool
	EnvdAuth        bool
	NonRootVMM      bool
	NetNS           bool
}

// CreateRequest mirrors the Linux type.
type CreateRequest struct {
	Template  string
	UserID    string
	Timeout   time.Duration
	VCPUs     int
	MemoryMB  int
	StorageMB int
	// Egress mirrors the linux field; this build boots nothing, so it is only
	// carried so the runtime adapter compiles off Linux.
	Egress  *egress.Rules
	Volumes []string
}

// Sandbox mirrors the fields other packages read from a created sandbox.
type Sandbox struct {
	ID        string
	Template  string
	UserID    string
	BootType  string
	BootMs    int64
	VMIP      string
	NodeAddr  string
	CreatedAt time.Time
	ExpiresAt time.Time
	Token     string
}

// Manager is a no-op stand-in; every VM operation returns ErrUnsupported.
type Manager struct {
	config Config
}

// NewManager never errors, so unit tests can construct a Manager.
func NewManager(cfg Config, _ *zap.Logger) (*Manager, error) {
	return &Manager{config: cfg}, nil
}

func (m *Manager) Create(context.Context, CreateRequest) (*Sandbox, error) {
	return nil, ErrUnsupported
}
func (m *Manager) Exec(context.Context, string, string, string, time.Duration) (*envd.ExecResult, error) {
	return nil, ErrUnsupported
}
func (m *Manager) ExecStream(context.Context, string, string, string, time.Duration, func(envd.StreamFrame)) error {
	return ErrUnsupported
}
func (m *Manager) ExecDetached(context.Context, string, string, string, time.Duration) (*envd.ExecResult, error) {
	return nil, ErrUnsupported
}
func (m *Manager) Destroy(context.Context, string) error                   { return ErrUnsupported }
func (m *Manager) DeliverIdentity(string, runtime.Identity) error          { return ErrUnsupported }
func (m *Manager) Alive(string) bool                                       { return true }
func (m *Manager) OOMKilled(string) bool                                   { return false }
func (m *Manager) RecordOwner(string, string) error                        { return nil }
func (m *Manager) AdoptOwned(string) (*Sandbox, bool)                      { return nil, false }
func (m *Manager) KillUnadopted([]string)                                  {}
func (m *Manager) GetSandboxInfo(string) (SandboxInfo, error)              { return SandboxInfo{}, ErrUnsupported }
func (m *Manager) ListUserSandboxes(string) []SandboxInfo                  { return nil }
func (m *Manager) WriteFile(context.Context, string, string, []byte) error { return ErrUnsupported }
func (m *Manager) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, ErrUnsupported
}
func (m *Manager) RefreshTTL(string, time.Duration) (time.Time, error) {
	return time.Time{}, ErrUnsupported
}
func (m *Manager) AuthorizeSandbox(string, string) error { return ErrUnsupported }
func (m *Manager) SnapshotSandbox(context.Context, string, string) error {
	return ErrUnsupported
}
func (m *Manager) RestoreSandbox(context.Context, string, string, int, int) (*Sandbox, error) {
	return nil, ErrUnsupported
}
func (m *Manager) DiskPressure() bool             { return false }
func (m *Manager) ActiveCount() int               { return 0 }
func (m *Manager) ActiveSandboxes() []SandboxInfo { return nil }
func (m *Manager) SetMaxConcurrent(int)           {}

// DiskUsagePercent always reports 0.
func DiskUsagePercent(string) float64 { return 0 }

// RunMonitors is a no-op.
func RunMonitors(context.Context, *Manager, *zap.Logger) {}

// PreflightIsolation mirrors the Linux canary-tap probe.
func PreflightIsolation() error { return ErrUnsupported }

// PreflightNetNS mirrors the Linux canary-netns probe.
func PreflightNetNS() error { return ErrUnsupported }

// SeccompEnabled is true so the probe never fails on a host that cannot run a VMM anyway.
func SeccompEnabled() bool { return true }

// ForkSource mirrors the linux type so callers compile everywhere.
type ForkSource struct {
	Template, StatePath, MemPath, OverlayPath, TapName, VMIP string
}

func (m *Manager) ForkSandbox(context.Context, string, string, int, int) (*Sandbox, time.Duration, error) {
	return nil, 0, ErrUnsupported
}
func (m *Manager) ForkFromSnapshot(context.Context, string, string, int, int) (*Sandbox, error) {
	return nil, ErrUnsupported
}

func (m *Manager) BootFromCapture(context.Context, string, string, int, int, *egress.Rules) (*Sandbox, error) {
	return nil, ErrUnsupported
}
func (m *Manager) DiscardSnapshot(string) error { return ErrUnsupported }

// SetNetworks mirrors the linux signature; this build partitions nothing.
func (m *Manager) SetNetworks(string, runtime.Networks) (string, error) {
	return "", ErrUnsupported
}

// TagPeers mirrors the linux helper; no sandbox here has peers.
func TagPeers(string) map[string]string { return nil }

func (m *Manager) CheckpointSandbox(context.Context, string, string) (time.Duration, int64, error) {
	return 0, 0, ErrUnsupported
}
func (m *Manager) RestoreCheckpoint(context.Context, string, string, int, int) (*Sandbox, error) {
	return nil, ErrUnsupported
}
func (m *Manager) ParkSandbox(context.Context, string) error { return ErrUnsupported }
func (m *Manager) FreezeVM(context.Context, string) error    { return ErrUnsupported }
func (m *Manager) ThawVM(context.Context, string) error      { return ErrUnsupported }

// CreateVolume and DeleteVolume mirror the linux signatures; this build has no VMs.
func (m *Manager) CreateVolume(string, int) error { return ErrUnsupported }
func (m *Manager) DeleteVolume(string) error      { return ErrUnsupported }
