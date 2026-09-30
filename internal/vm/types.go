//go:build linux

package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
	"time"
)

// FirecrackerMachine abstracts the Firecracker SDK's Machine type so tests can mock it.
type FirecrackerMachine interface {
	PauseVM(ctx context.Context) error
	ResumeVM(ctx context.Context) error
	CreateSnapshot(ctx context.Context, statePath, memPath string) error
	// CreateDiffSnapshot writes only the pages dirtied since the VM was
	// restored (or since its last diff); memPath is sparse where pages are clean.
	CreateDiffSnapshot(ctx context.Context, statePath, memPath string) error
	Stop(ctx context.Context) error
	Pid() int
	// ChrootRoot is the host path of the jailer chroot's root dir, or "" without
	// a jailer. Derived from the actual jailer id, which is not always the sandbox id.
	ChrootRoot() string
}

// RestoredVM is a running Firecracker VM ready for use.
type RestoredVM struct {
	Machine     FirecrackerMachine
	VsockPath   string
	OverlayPath string
	TapName     string
	VMIP        string
	BootType    string // "snapshot" | "cold"
	// NetNS is the network namespace name, "" on the shared-bridge path.
	NetNS string
	// MemBase is the memory file this VM was restored from and tracks dirty
	// pages against; "" for a cold boot.
	MemBase string
}

// Destroy tears down all resources associated with this VM.
func (vm *RestoredVM) Destroy() {
	if vm.Machine != nil {
		_ = vm.Machine.Stop(context.Background())
		cleanJailerRoot(vm.Machine.ChrootRoot())
	}
	deleteOverlay(vm.OverlayPath)
	if vm.NetNS != "" {
		teardownNetNS(vm.NetNS)
		return
	}
	TeardownVMNetwork(vm.TapName)
}

// FirecrackerConfig holds all parameters needed to start or restore a Firecracker VM.
type FirecrackerConfig struct {
	// LazyMem restores through the Uffd backend (lazymem.go) instead of mapping
	// the memory file. Never for a golden restore: those map one shared file
	// that the page cache serves every VM from, and a userfaultfd would give
	// each VM a private copy of all of it.
	LazyMem bool
	// MemSource serves the memory image when there is no file for it: a
	// capture whose memory stayed in the bucket (mem.lazy). Implies LazyMem.
	MemSource runtime.MemSource
	SandboxID string
	Template  string
	// RootfsPath is the read-only root image to boot, overriding the one the
	// template NAME resolves to. A capture that carries its own image sets it:
	// the name resolves to whatever this node happens to have built, which is
	// not what the capture was taken against. Empty falls back to the name.
	RootfsPath  string
	TapName     string
	VMIP        interface{} // net.IP
	OverlayPath string
	StatePath   string // empty = cold boot
	MemPath     string
	VCPUs       int // 0 means Config.VCPUs, then 1
	MemoryMB    int // 0 means Config.MemoryMB, then 1024
	// TokenHash, when set, rides the kernel cmdline as envd.token_hash=<hash>. Never the token.
	TokenHash string
	// NetNS is the namespace path handed to the jailer as --netns; "" keeps it in the root netns.
	NetNS  string
	Config Config
}

// OrphanProcess is a Firecracker process from a previous run.
type OrphanProcess struct {
	SandboxID   string
	VsockPath   string
	OverlayPath string
	Machine     FirecrackerMachine
}

// Kill terminates the orphaned VMM and reports why if it could not.
func (p *OrphanProcess) Kill() error {
	if p.Machine == nil {
		return nil
	}
	return p.Machine.Stop(context.Background())
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateSandboxID() string {
	ts := time.Now().UnixMilli()
	b := make([]byte, 4)
	rand.Read(b)
	// Hyphens, not underscores: the jailer rejects '_' in --id.
	return fmt.Sprintf("sb-%d-%x", ts, b)
}
