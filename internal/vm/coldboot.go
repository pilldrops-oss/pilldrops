//go:build linux

package vm

import (
	"context"
	"fmt"
	stdnet "net"
	"os"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// ColdBoot starts a fresh VM from the squashfs rootfs. vcpu/mem of 0 fall back
// to the config default.
func ColdBoot(template, sandboxID string, cfg Config, log *zap.Logger, vcpu, mem, storageMB int, tokenHash string, rules *egress.Rules) (*RestoredVM, error) {
	log.Info("cold_boot",
		zap.String("template", template),
		zap.String("sandbox_id", sandboxID),
		zap.Int("vcpu", vcpu),
		zap.Int("mem_mb", mem),
	)

	// The overlay owner, the tap owner, and the jailer uid must all be this
	// one uid or the dropped-privilege VMM cannot open them.
	uid := vmUID(cfg.NonRootVMM, sandboxID)

	// FC_NETNS: each sandbox in its own netns with a fixed tap/MAC/guest IP.
	if cfg.NetNS {
		return coldBootNetNS(template, sandboxID, cfg, log, uid, vcpu, mem, storageMB, tokenHash, rules)
	}

	// Shared-bridge mode has nowhere to put a per-sandbox rule: the host chains
	// see only the post-SNAT source. Booting anyway would hand back a sandbox
	// that reads as confined and reaches everything.
	if rules != nil {
		return nil, fmt.Errorf("this node runs sandboxes on the shared bridge, which cannot "+
			"enforce a per-sandbox egress policy; run with FC_NETNS=1 or create %s without one", sandboxID)
	}

	net, err := SetupVMNetwork(sandboxID, uid)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}

	overlayPath, err := createOverlay(sandboxID, uid, storageMB, cfg)
	if err != nil {
		TeardownVMNetwork(net.TapName)
		return nil, fmt.Errorf("overlay: %w", err)
	}

	machine, err := startFirecracker(context.Background(), FirecrackerConfig{
		SandboxID:   sandboxID,
		Template:    template,
		TapName:     net.TapName,
		VMIP:        net.VMIP,
		OverlayPath: overlayPath,
		VCPUs:       vcpu,
		MemoryMB:    mem,
		TokenHash:   tokenHash, // "" unless envd auth is enabled
		// StatePath empty = full kernel boot, not snapshot restore
		Config: cfg,
	})
	if err != nil {
		TeardownVMNetwork(net.TapName)
		os.Remove(overlayPath)
		return nil, fmt.Errorf("firecracker start: %w", err)
	}

	return &RestoredVM{
		Machine: machine,
		// envd is reached over TCP; vsock carries only the credential channel.
		// Moving gRPC to vsock needs the CONNECT handshake in this dialer first.
		VsockPath:   net.VMIP.String() + ":6001",
		OverlayPath: overlayPath,
		TapName:     net.TapName,
		VMIP:        net.VMIP.String(),
		BootType:    "cold",
	}, nil
}

// coldBootNetNS is ColdBoot with a per-sandbox network namespace. The node
// reaches envd at ns.NodeAddr, which the netns DNATs onto the fixed guest IP.
func coldBootNetNS(template, sandboxID string, cfg Config, log *zap.Logger, uid, vcpu, mem, storageMB int, tokenHash string, rules *egress.Rules) (*RestoredVM, error) {
	ns, err := setupNetNS(sandboxID, uid, netnsTapName, stdnet.ParseIP(netnsGuestIP), rules, log)
	if err != nil {
		return nil, fmt.Errorf("netns: %w", err)
	}

	overlayPath, err := createOverlay(sandboxID, uid, storageMB, cfg)
	if err != nil {
		teardownNetNS(ns.Name)
		return nil, fmt.Errorf("overlay: %w", err)
	}

	machine, err := startFirecracker(context.Background(), FirecrackerConfig{
		SandboxID:   sandboxID,
		Template:    template,
		TapName:     ns.TapName, // fixed "tap0", inside the namespace
		VMIP:        ns.VMIP,    // fixed 192.168.30.2
		OverlayPath: overlayPath,
		VCPUs:       vcpu,
		MemoryMB:    mem,
		TokenHash:   tokenHash,
		NetNS:       ns.NetNSPath,
		Config:      cfg,
	})
	if err != nil {
		teardownNetNS(ns.Name)
		os.Remove(overlayPath)
		return nil, fmt.Errorf("firecracker start (netns): %w", err)
	}

	return &RestoredVM{
		Machine:     machine,
		VsockPath:   ns.NodeAddr, // host-reachable; DNAT'd onto the fixed guest IP
		OverlayPath: overlayPath,
		TapName:     ns.TapName,
		VMIP:        ns.VMIP.String(),
		BootType:    "cold",
		NetNS:       ns.Name,
	}, nil
}
