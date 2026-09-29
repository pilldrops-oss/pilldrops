//go:build linux

package vm

// The live half of tag networks: the node-global membership table, the nft
// calls that make it real, and the in-namespace re-render a sandbox needs when
// it joins or leaves.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/vishvananda/netns"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// tagMembers is keyed by the vm-layer sandbox id. Package-level, like
// netnsSlotUsed, because teardownNetNS is package-level: the membership has to
// die with the namespace whether or not a Manager is in scope.
var (
	tagMu      sync.Mutex
	tagMembers = map[string]tagMember{}
)

// applyPeerSetLocked pushes the whole membership into the host set. One nft
// file, so it loads atomically. Called with tagMu held.
func applyPeerSetLocked() error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(renderPeerSet(tagMembers))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft peer set: %v: %s", err, bytes.TrimSpace(out))
	}
	ensurePeerAcceptRule()
	return nil
}

// ensurePeerAcceptRule re-inserts the rule that reads the peer set when it is
// missing. The host table is node-global: another netns daemon starting on the
// same box recreates it, and the failure is silent and one-directional — peers
// simply stop connecting, with a full set and no rule to consult it. `insert`
// with no index puts it back at the top of the chain, ahead of the slot-to-slot
// drop, which is the only position that means anything.
func ensurePeerAcceptRule() {
	out, err := exec.Command("nft", "list", "chain", "ip", netnsHostTable, "forward").Output()
	if err == nil && bytes.Contains(out, []byte("@"+peerSetName+" accept")) {
		return
	}
	_ = exec.Command("nft", "insert", "rule", "ip", netnsHostTable, "forward",
		"ip", "saddr", ".", "ip", "daddr", "@"+peerSetName, "accept").Run()
}

// forgetTagMemberByNS drops the sandbox owning a namespace and re-renders the
// host set. Best effort by design: it runs on teardown paths that cannot fail,
// and the alternative to a logged error is a leaked element.
func forgetTagMemberByNS(nsName string) {
	tagMu.Lock()
	defer tagMu.Unlock()
	var gone []string
	for id, l := range tagLive {
		if l.ns == nsName {
			gone = append(gone, id)
		}
	}
	if len(gone) == 0 {
		return
	}
	for _, id := range gone {
		if l := tagLive[id]; l != nil && l.dns != nil {
			_ = l.dns.Close()
		}
		delete(tagLive, id)
		delete(tagMembers, id)
	}
	_ = applyPeerSetLocked()
}

// SetNetworks replaces a sandbox's network membership and returns the address
// peers reach it on ("" when it joined none). It re-renders the host peer set
// and, when the membership changed between "none" and "some", the sandbox's own
// in-namespace ruleset: the in-ns 10.0.0.0/8 drop is the second half of peer
// isolation and would otherwise drop the traffic the host chain now admits.
func (m *Manager) SetNetworks(id string, n rt.Networks) (string, error) {
	m.mu.RLock()
	sb, ok := m.active[id]
	m.mu.RUnlock()
	if !ok {
		return "", ErrSandboxNotFound
	}
	if sb.NetNS == "" {
		return "", fmt.Errorf("%w: tag networks need per-sandbox namespaces (FC_NETNS=1)", rt.ErrUnsupported)
	}
	slot, _, okName := parseNetNSName(sb.NetNS)
	if !okName {
		return "", fmt.Errorf("namespace %q does not carry a slot", sb.NetNS)
	}
	ip := slotIP(slot, 2).String()

	tagMu.Lock()
	was := len(tagMembers[id].sets) > 0
	now := len(n.Sets) > 0
	if now {
		tagMembers[id] = tagMember{sets: append([]string(nil), n.Sets...), slug: n.Slug, ip: ip}
		if tagLive[id] == nil {
			tagLive[id] = &liveTag{ns: sb.NetNS}
		}
		tagLive[id].ns = sb.NetNS
	} else {
		delete(tagMembers, id)
	}
	err := applyPeerSetLocked()
	tagMu.Unlock()
	if err != nil {
		return "", err
	}

	// Only when the sandbox crossed the tagged/untagged line: re-rendering
	// flushes and reloads the namespace's ruleset, and doing it on every slug
	// change would be a needless window on a live sandbox.
	// The resolver and the redirect that points the guest at it move together
	// with the ruleset.
	if was != now {
		if err := m.reapplyNetNSRuleset(sb, slot, now); err != nil {
			return "", err
		}
		if now {
			if err := startResolver(id, sb.NetNS); err != nil {
				return "", err
			}
		} else {
			stopResolver(id)
		}
	}
	if !now {
		return "", nil
	}
	return ip, nil
}

// reapplyNetNSRuleset re-renders and reloads a running sandbox's in-namespace
// ruleset with its current tagged state. The egress policy is re-rendered from
// the same rules the boot used, so a policied sandbox keeps its policy.
func (m *Manager) reapplyNetNSRuleset(sb *Sandbox, slot int, tagged bool) error {
	policy, divert, err := renderEgressPolicy(sb.Egress, sb.TapName)
	if err != nil {
		return err
	}
	hostIP, nsIP := slotIP(slot, 1), slotIP(slot, 2)
	guestIP := net.ParseIP(sb.VMIP)
	if guestIP == nil {
		return fmt.Errorf("sandbox %s has no usable VM IP %q", sb.ID, sb.VMIP)
	}
	ruleset := buildNetNSRuleset(vethNS(slot), sb.TapName, hostIP, nsIP, guestIP, policy, divert, tagged)
	return applyRulesetInNS(netnsRunDir+"/"+sb.NetNS, ruleset)
}

// TagPeers returns the slug-to-address map a sandbox's own resolver may answer.
func TagPeers(id string) map[string]string {
	tagMu.Lock()
	defer tagMu.Unlock()
	return peersOf(tagMembers, id)
}

// liveTag is the part of a membership that only exists on a running node: the
// namespace it lives in and the resolver socket open inside it. Kept apart from
// tagMember so the rendering type stays pure and testable off Linux.
type liveTag struct {
	ns  string
	dns net.PacketConn
}

// tagLive is keyed like tagMembers and guarded by the same mutex.
var tagLive = map[string]*liveTag{}

// startResolver opens this namespace's resolver on the tap gateway and serves
// it. The socket is created *inside* the namespace — a netns is a property of
// the file descriptor, so it keeps that namespace once the thread returns —
// which is what lets it answer without any host chain exception and stay
// reachable under an egress policy.
func startResolver(id, nsName string) error {
	tagMu.Lock()
	running := tagLive[id] != nil && tagLive[id].dns != nil
	tagMu.Unlock()
	if running {
		return nil
	}
	pc, err := listenInNS(netnsRunDir+"/"+nsName, net.JoinHostPort(netnsTapGW, "53"))
	if err != nil {
		return fmt.Errorf("netns %s: private resolver: %w", nsName, err)
	}
	tagMu.Lock()
	if tagLive[id] == nil {
		tagLive[id] = &liveTag{ns: nsName}
	}
	tagLive[id].dns = pc
	tagMu.Unlock()
	go serveDNS(pc, func() map[string]string { return TagPeers(id) }, guestResolvers)
	return nil
}

// stopResolver closes a sandbox's resolver, which ends its serve loop.
func stopResolver(id string) {
	tagMu.Lock()
	var pc net.PacketConn
	if l := tagLive[id]; l != nil {
		pc, l.dns = l.dns, nil
	}
	tagMu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
}

// listenInNS opens a UDP socket inside a namespace, and listenTCPInNS a TCP
// listener. A netns is a property of the file descriptor, so the socket keeps
// that namespace once the thread has come back.
func listenInNS(nsPath, addr string) (net.PacketConn, error) {
	var pc net.PacketConn
	err := inNS(nsPath, func() (io.Closer, error) {
		var err error
		pc, err = net.ListenPacket("udp", addr)
		return pc, err
	})
	return pc, err
}

func listenTCPInNS(nsPath, addr string) (net.Listener, error) {
	var l net.Listener
	err := inNS(nsPath, func() (io.Closer, error) {
		var err error
		l, err = net.Listen("tcp", addr)
		return l, err
	})
	return l, err
}

// inNS runs open inside a namespace. Same locked-thread dance as
// createNamespacedTap: enter, open, come back, and never unlock the thread if
// the return failed, so the runtime retires it instead of scheduling other
// goroutines in the wrong namespace. open returns what it created so a socket
// that outlived a failed return can be closed rather than leaked into a
// namespace nothing holds a handle to.
func inNS(nsPath string, open func() (io.Closer, error)) error {
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("current namespace: %w", err)
	}
	defer orig.Close()
	target, err := netns.GetFromPath(nsPath)
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("open %s: %w", nsPath, err)
	}
	defer target.Close()
	if err := netns.Set(target); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("enter %s: %w", nsPath, err)
	}
	c, openErr := open()
	if err := netns.Set(orig); err != nil {
		if c != nil {
			_ = c.Close()
		}
		return fmt.Errorf("LEAKED THREAD, could not return to the host namespace: %v (open err: %v)", err, openErr)
	}
	runtime.UnlockOSThread()
	return openErr
}
