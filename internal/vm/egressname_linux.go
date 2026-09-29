//go:build linux

package vm

// The name layer's lifecycle. One TCP listener inside a sandbox's own network
// namespace when its policy names a domain, and nothing at all when it does
// not: a rule set of addresses and CIDRs is decided entirely in the kernel and
// never takes a userspace hop.
//
// The listener is opened INSIDE the namespace for the same reason the private
// resolver is (startResolver): a netns is a property of the file descriptor, so
// the socket keeps that namespace and the guest's `redirect` finds it without a
// single host-side exception. The decision and the splice live in
// internal/egressproxy; nothing about the policy is decided here.

import (
	"fmt"
	"net"
	"net/netip"
	"sync"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/egressproxy"
)

// nameLayer holds the listeners by namespace name, so teardownNetNS — the one
// path every destroy, failed boot and startup reclaim passes through — closes
// them without needing a Manager in scope. Same shape, and the same reason, as
// tagLive.
var (
	nameMu     sync.Mutex
	nameLayers = map[string]net.Listener{}
)

// startNameLayer opens a sandbox's name layer, or does nothing when its rules
// name no domain. An error fails the boot: the divert rule is already rendered
// by then, so a namespace whose proxy did not start would send every 80/443
// connection at a closed port. That fails closed, but as a sandbox that reads
// as confined by rules nothing is evaluating, which is the failure this whole
// layer exists to stop.
func startNameLayer(nsName, nsPath, sandboxID string, rules *egress.Rules, log *zap.Logger) error {
	if !rules.HasDomains() {
		return nil
	}
	allow, deny, err := rules.Parse()
	if err != nil {
		return fmt.Errorf("egress rules: %w", err)
	}
	addr := net.JoinHostPort(netnsTapGW, fmt.Sprint(egressProxyPort))
	l, err := listenTCPInNS(nsPath, addr)
	if err != nil {
		return fmt.Errorf("netns %s: name layer on %s: %w", nsName, addr, err)
	}

	nameMu.Lock()
	if old := nameLayers[nsName]; old != nil {
		_ = old.Close()
	}
	nameLayers[nsName] = l
	nameMu.Unlock()

	self, _ := netip.ParseAddrPort(addr)
	go egressproxy.Serve(l, egressproxy.Policy{
		Allow: allow, Deny: deny, SandboxID: sandboxID, Self: self,
		Record: func(c egress.Conn, v egress.Verdict) {
			// Both verdicts, not only denials: a policy nobody can see working
			// is indistinguishable from one that is not running.
			lvl := log.Info
			if !v.Allowed {
				lvl = log.Warn
			}
			lvl("egress_name_verdict",
				zap.String("sandbox_id", sandboxID),
				zap.String("name", c.Name),
				zap.String("dst", c.Addr.String()),
				zap.Int("port", c.Port),
				zap.Bool("allowed", v.Allowed),
				zap.String("reason", v.Reason))
		},
	})
	log.Info("egress_name_layer_started", zap.String("sandbox_id", sandboxID),
		zap.String("netns", nsName), zap.String("addr", addr))
	return nil
}

// stopNameLayer closes a sandbox's listener, which ends its accept loop. Live
// spliced connections die with the namespace.
func stopNameLayer(nsName string) {
	nameMu.Lock()
	l := nameLayers[nsName]
	delete(nameLayers, nsName)
	nameMu.Unlock()
	if l != nil {
		_ = l.Close()
	}
}
