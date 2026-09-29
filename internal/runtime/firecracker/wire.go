package firecracker

import (
	"context"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/vm"

	"go.uber.org/zap"
)

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
	Resources       map[string]rt.Resources
	EnableSnapshots bool
	EnvdAuth        bool
	NonRootVMM      bool
	// NetNS boots every sandbox in its own network namespace (FC_NETNS).
	NetNS  bool
	Logger *zap.Logger
}

// New builds the Firecracker runtime. ctx is the node's lifetime: the manager's
// background monitors run under it.
func New(ctx context.Context, cfg Config) (rt.Runtime, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	mgr, err := vm.NewManager(vm.Config{
		SnapshotDir:     cfg.SnapshotDir,
		OverlayDir:      cfg.OverlayDir,
		FirecrackerBin:  cfg.FirecrackerBin,
		JailerBin:       cfg.JailerBin,
		KernelPath:      cfg.KernelPath,
		Templates:       cfg.Templates,
		MaxConcurrent:   cfg.MaxConcurrent,
		MaxActive:       cfg.MaxActive,
		MemoryMB:        cfg.MemoryMB,
		VCPUs:           cfg.VCPUs,
		Resources:       cfg.Resources,
		EnableSnapshots: cfg.EnableSnapshots,
		EnvdAuth:        cfg.EnvdAuth,
		NonRootVMM:      cfg.NonRootVMM,
		NetNS:           cfg.NetNS,
	}, logger)
	if err != nil {
		return nil, err
	}
	// The disk watchdog is the only writer of the flag the create and fork paths
	// read to refuse work above 80% full; unstarted, that guard reads as
	// protection and is wired to nothing.
	vm.RunMonitors(ctx, mgr, logger)
	r := newWithBackend(mgr)
	r.netns = cfg.NetNS
	// Golden restore serves exactly one storage size, and only when snapshots are on.
	if cfg.EnableSnapshots {
		r.warmRestoreStorageMB = vm.DefaultOverlayMB
	}
	return r, nil
}
