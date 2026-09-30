//go:build linux

package vm

// Per-sandbox network namespaces. Every sandbox has the same tap name, guest IP
// and MAC inside its namespace, so a golden snapshot's baked identity fits all.
// The host reaches each guest over a per-slot /30 veth pair with DNAT to the guest IP.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// The identity every sandbox has inside its own netns. setupNetNS takes tap name
// and guest IP as parameters so a bridge-built golden that baked a different
// identity still restores.
const (
	netnsGuestIP = "192.168.30.2"
	netnsTapGW   = "192.168.30.1"
	netnsTapName = "tap0"
)

const (
	netnsPrefix = "fcns-"
	netnsRunDir = "/var/run/netns"
	// netnsMaxSlots is 10.128.0.0/16 packed as consecutive /30s.
	netnsMaxSlots = 16384
)

// netnsSlotUsed is process-local but never the only record: the slot is embedded
// in the netns and veth names, so reclaimNetNS reseeds it at startup.
var (
	netnsSlotMu   sync.Mutex
	netnsSlotUsed = map[int]bool{}
)

func allocateNetNSSlot() (int, error) {
	netnsSlotMu.Lock()
	defer netnsSlotMu.Unlock()
	for s := netnsSlotBase(); s < netnsMaxSlots; s++ {
		if netnsSlotUsed[s] {
			continue
		}
		// The host veth is the cross-process truth: a co-hosted daemon (a
		// side-car on the same node) may hold this slot, and only its link
		// name says so. Skipped, not claimed, so it is re-checked next time.
		if _, err := os.Stat("/sys/class/net/" + vethHost(s)); err == nil {
			continue
		}
		netnsSlotUsed[s] = true
		return s, nil
	}
	return 0, fmt.Errorf("no free netns slots (max %d)", netnsMaxSlots)
}

func releaseNetNSSlot(s int) {
	netnsSlotMu.Lock()
	delete(netnsSlotUsed, s)
	netnsSlotMu.Unlock()
}

func markNetNSSlotUsed(s int) {
	netnsSlotMu.Lock()
	netnsSlotUsed[s] = true
	netnsSlotMu.Unlock()
}

// slotIP returns 10.128.0.0 + slot*4 + off; off 1 is the host veth end, off 2 the ns end.
func slotIP(slot, off int) net.IP {
	n := slot*4 + off
	return net.IPv4(10, 128, byte(n>>8), byte(n&0xff))
}

// netnsNameFor embeds the slot in the name: it is the only durable record of the
// slot binding, so teardown and reclaim can rebuild veth names from it alone.
func netnsNameFor(slot int, sandboxID string) string {
	return fmt.Sprintf("%s%d-%s", netnsPrefix, slot, sandboxID)
}

// parseNetNSName inverts netnsNameFor; ok is false for names this package did not write.
func parseNetNSName(name string) (slot int, sandboxID string, ok bool) {
	rest, found := strings.CutPrefix(name, netnsPrefix)
	if !found {
		return 0, "", false
	}
	slotStr, id, found := strings.Cut(rest, "-")
	if !found || id == "" {
		return 0, "", false
	}
	s, err := strconv.Atoi(slotStr)
	if err != nil || s < 0 || s >= netnsMaxSlots {
		return 0, "", false
	}
	return s, id, true
}

func vethHost(slot int) string { return fmt.Sprintf("vh%d", slot) }
func vethNS(slot int) string   { return fmt.Sprintf("vn%d", slot) }

// SetupNetNSResult is what a netns boot returns in place of NetworkSetup.
type SetupNetNSResult struct {
	Name      string // fcns-<slot>-<sandboxID>
	NetNSPath string // /var/run/netns/<name>, handed to the jailer as --netns
	TapName   string
	VMIP      net.IP
	NodeAddr  string // host-reachable "ip:6001" the node dials to reach envd
}

// setupNetNS builds a per-sandbox namespace: tap, /30 veth to the host, default
// route and the in-ns nftables ruleset. Exec calls are slow, so everything is
// in-process netlink except one nft child for the whole ruleset.
func setupNetNS(sandboxID string, uid int, tapName string, guestIP net.IP, rules *egress.Rules, log *zap.Logger) (result *SetupNetNSResult, err error) {
	// Rendered before anything is created: a policy this node cannot enforce
	// must fail the boot, never come up as a sandbox that reads as confined.
	policy, divert, err := renderEgressPolicy(rules, tapName)
	if err != nil {
		return nil, err
	}
	slot, err := allocateNetNSSlot()
	if err != nil {
		return nil, err
	}
	name := netnsNameFor(slot, sandboxID)
	defer func() {
		if err != nil {
			teardownNetNS(name)
		}
	}()

	hostIP, nsIP := slotIP(slot, 1), slotIP(slot, 2)
	vh, vn := vethHost(slot), vethNS(slot)

	nsHandle, err := createNamespacedTap(name, tapName, uid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = nsHandle.Close() }()

	veth := &netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: vh},
		PeerName:      vn,
		PeerNamespace: netlink.NsFd(nsHandle),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, fmt.Errorf("netns %s: create veth %s/%s: %w", name, vh, vn, err)
	}
	hostAddr, err := netlink.ParseAddr(hostIP.String() + "/30")
	if err != nil {
		return nil, fmt.Errorf("netns %s: parse host addr: %w", name, err)
	}
	if err := netlink.AddrAdd(veth, hostAddr); err != nil {
		return nil, fmt.Errorf("netns %s: addr %s on %s: %w", name, hostAddr, vh, err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		return nil, fmt.Errorf("netns %s: %s up: %w", name, vh, err)
	}

	nsl, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		return nil, fmt.Errorf("netns %s: netlink handle: %w", name, err)
	}
	defer nsl.Close()
	vnLink, err := nsl.LinkByName(vn)
	if err != nil {
		return nil, fmt.Errorf("netns %s: %s not in namespace: %w", name, vn, err)
	}
	nsAddr, err := netlink.ParseAddr(nsIP.String() + "/30")
	if err != nil {
		return nil, fmt.Errorf("netns %s: parse ns addr: %w", name, err)
	}
	if err := nsl.AddrAdd(vnLink, nsAddr); err != nil {
		return nil, fmt.Errorf("netns %s: addr %s on %s: %w", name, nsAddr, vn, err)
	}
	if err := nsl.LinkSetUp(vnLink); err != nil {
		return nil, fmt.Errorf("netns %s: %s up: %w", name, vn, err)
	}
	if err := nsl.RouteAdd(&netlink.Route{Gw: hostIP}); err != nil {
		return nil, fmt.Errorf("netns %s: default route via %s: %w", name, hostIP, err)
	}

	// Before the ruleset, not after: the divert sends 80/443 at a port that
	// must already be listening, or the first connection out of a sandbox with
	// a domain rule is refused by its own namespace. A failure here fails the
	// boot through the deferred teardown.
	if err := startNameLayer(name, netnsRunDir+"/"+name, sandboxID, rules, log); err != nil {
		return nil, err
	}

	// Not optional: host chains only see the post-SNAT source, so every egress
	// control must live in here. A new namespace is always untagged: membership
	// is pushed after the boot, by SetNetworks, which re-renders this.
	ruleset := buildNetNSRuleset(vn, tapName, hostIP, nsIP, guestIP, policy, divert, false)
	if err := applyRulesetInNS("/var/run/netns/"+name, ruleset); err != nil {
		return nil, err
	}

	return &SetupNetNSResult{
		Name:      name,
		NetNSPath: netnsRunDir + "/" + name,
		TapName:   tapName,
		VMIP:      guestIP,
		NodeAddr:  nsIP.String() + ":6001",
	}, nil
}

// createNamespacedTap creates the namespace and, inside it, the tap, loopback,
// gateway address and ip_forward. Tun ioctls and /proc/sys act on the calling
// thread's netns, so this is the one step that must enter the namespace.
func createNamespacedTap(name, tapName string, uid int) (netns.NsHandle, error) {
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return netns.None(), fmt.Errorf("netns %s: current namespace: %w", name, err)
	}
	defer func() { _ = orig.Close() }()

	nsHandle, err := netns.NewNamed(name) // creates AND enters
	if err != nil {
		runtime.UnlockOSThread()
		return netns.None(), fmt.Errorf("netns %s: create: %w", name, err)
	}

	inErr := func() error {
		if lo, err := netlink.LinkByName("lo"); err == nil {
			if err := netlink.LinkSetUp(lo); err != nil {
				return fmt.Errorf("lo up: %w", err)
			}
		}
		tap := buildTuntap(tapName, uid)
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("create tap %s: %w", tapName, err)
		}
		gw, err := netlink.ParseAddr(netnsTapGW + "/16")
		if err != nil {
			return fmt.Errorf("parse gateway addr: %w", err)
		}
		if err := netlink.AddrAdd(tap, gw); err != nil {
			return fmt.Errorf("addr %s on tap: %w", gw, err)
		}
		if err := netlink.LinkSetUp(tap); err != nil {
			return fmt.Errorf("tap up: %w", err)
		}
		if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
			return fmt.Errorf("ip_forward: %w", err)
		}
		return nil
	}()

	if restoreErr := netns.Set(orig); restoreErr != nil {
		// Leave the thread locked so the runtime retires it instead of scheduling
		// another goroutine in the wrong namespace.
		_ = nsHandle.Close()
		return netns.None(), fmt.Errorf("netns %s: LEAKED THREAD, could not return to host namespace: %w (setup err: %v)", name, restoreErr, inErr) //nolint:errorlint // inErr is nil when setup succeeded; %w would print %!w(<nil>)
	}
	runtime.UnlockOSThread()

	if inErr != nil {
		_ = nsHandle.Close()
		return netns.None(), fmt.Errorf("netns %s: %w", name, inErr)
	}
	return nsHandle, nil
}

// buildNetNSHostRuleset renders the host half: supernet masquerade, sandbox-to-host
// new-connection drop, slot-to-slot drop. The input drop is keyed on iifname "vh*"
// too, because the host's own traffic to a veth address arrives over lo with a 10.128 source.
func buildNetNSHostRuleset(egressIf string) string {
	masq := "		ip saddr " + netnsSupernet + " masquerade\n"
	if egressIf != "" {
		masq = fmt.Sprintf("		ip saddr "+netnsSupernet+" oifname %q masquerade\n", egressIf)
	}
	// The peer set is declared empty and the accept rule sits ahead of the
	// slot-to-slot drop. Empty, it matches nothing, so this table's behaviour
	// with no tagged sandbox on the node is the behaviour it had before tag
	// networks existed; SetNetworks fills the set, and nothing else does.
	return fmt.Sprintf(`table ip %[1]s
delete table ip %[1]s
table ip %[1]s {
	set %[4]s {
		type ipv4_addr . ipv4_addr
	}
	chain postrouting {
		type nat hook postrouting priority srcnat;
%[2]s	}
	chain input {
		type filter hook input priority filter; policy accept;
		ip saddr %[5]s ct state established,related accept
%[3]s		iifname "vh*" ip saddr %[5]s drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		ip saddr . ip daddr @%[4]s accept
		ip saddr %[5]s ip daddr %[5]s drop
		ip saddr %[5]s ip daddr 169.254.0.0/16 drop
	}
}
`, netnsHostTable, masq, localAPIHostRule(), peerSetName, netnsSupernet)
}

// baseFilterChain is where netnsBaseChainRules belong. DOCKER-USER when docker
// has created it: a docker daemon restart rewrites FORWARD (that is how its
// policy became DROP in the first place) and DOCKER-USER is the chain it
// promises to leave alone. Plain FORWARD on a node with no docker, where a DROP
// policy is someone's own hardening and the rules still have to survive it.
func baseFilterChain() string {
	if exec.CommandContext(context.Background(), "iptables", "-S", "DOCKER-USER").Run() == nil {
		return "DOCKER-USER"
	}
	return "FORWARD"
}

// allowNetNSPastBaseChains installs those rules, idempotently. Best effort and
// quiet: a node whose iptables refuses is a node the boot preflight fails by
// reading the chain back, which is worth more than a log line nobody greps.
// Not persisted to /etc/iptables/rules.v4 — it re-runs on every sandboxd start,
// so it survives a reboot through the daemon, not through the restore unit.
func allowNetNSPastBaseChains() {
	for _, r := range netnsBaseChainRules(baseFilterChain()) {
		if exec.CommandContext(context.Background(), "iptables", append([]string{"-C"}, r...)...).Run() == nil {
			continue
		}
		_ = exec.CommandContext(context.Background(), "iptables", append([]string{"-I"}, r...)...).Run()
	}
}

// applyRulesetInNS loads a ruleset inside a namespace. nsenter rather than
// `ip netns exec`, which also unshares a mount namespace and remounts /sys.
func applyRulesetInNS(nsPath, ruleset string) error {
	cmd := exec.CommandContext(context.Background(), "nsenter", "--net="+nsPath, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft in %s: %w: %s", nsPath, err, bytes.TrimSpace(out))
	}
	return nil
}

// EnsureNetNSHostRules installs the host-side tables and sets the host's
// ip_forward, without which every guest packet is silently dropped.
func EnsureNetNSHostRules() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
		return fmt.Errorf("host ip_forward (netns mode forwards every guest packet through the host namespace): %w", err)
	}
	cmd := exec.CommandContext(context.Background(), "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(buildNetNSHostRuleset(detectEgressIf()))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("host nft table %s: %w: %s", netnsHostTable, err, bytes.TrimSpace(out))
	}
	// After the table loads, and in this order: this table decides nothing on a
	// node whose base filter FORWARD chain drops the traffic first, and this is
	// the one place the node's netns rules are established.
	allowNetNSPastBaseChains()
	if !hostHasIPv6() {
		return nil
	}
	cmd6 := exec.CommandContext(context.Background(), "nft", "-f", "-")
	cmd6.Stdin = strings.NewReader(buildNetNSHostRuleset6())
	if out, err := cmd6.CombinedOutput(); err != nil {
		return fmt.Errorf("host nft table %s (the IPv6 backstop; this host HAS an IPv6 stack, "+
			"so refusing to run without it rather than forwarding an unfiltered address family): %v: %s",
			netnsHostTable6, err, bytes.TrimSpace(out))
	}
	return nil
}

// hostHasIPv6 reports whether the kernel has IPv6; without it the ip6 table
// cannot load and no IPv6 packet can cross a veth.
func hostHasIPv6() bool {
	_, err := os.Stat("/proc/net/if_inet6")
	return err == nil
}

// netnsHostTable6 is the IPv6 counterpart of netnsHostTable.
const netnsHostTable6 = "sandboxd_netns6"

// buildNetNSHostRuleset6 drops all IPv6 on sandbox veths. Every other rule is
// `table ip`, so a guest with a link-local address would otherwise reach the
// host's :: listeners; guests boot with ipv6.disable=1 but this is the backstop.
func buildNetNSHostRuleset6() string {
	return fmt.Sprintf(`table ip6 %[1]s
delete table ip6 %[1]s
table ip6 %[1]s {
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "vh*" drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "vh*" drop
		oifname "vh*" drop
	}
}
`, netnsHostTable6)
}

// teardownNetNS removes a namespace, its veth pair and its slot reservation.
// The host veth end is deleted explicitly so a namespace that lost its peer cannot leak it.
func teardownNetNS(name string) {
	// Before the slot is released: this is the one place every teardown passes
	// through (destroy, a failed boot, reclaim at startup), so dropping the
	// membership here is what makes a reclaimed slot unreachable by its
	// predecessor's peers. A caller that forgot to clear it cannot leave one.
	forgetTagMemberByNS(name)
	// Same argument: the name layer's listener lives in this namespace, and a
	// listener outliving it would hold a socket in a namespace nothing can
	// reach and a policy for a sandbox that no longer exists.
	stopNameLayer(name)
	if slot, _, ok := parseNetNSName(name); ok {
		if link, err := netlink.LinkByName(vethHost(slot)); err == nil {
			_ = netlink.LinkDel(link)
		}
		defer releaseNetNSSlot(slot)
	}
	_ = netns.DeleteNamed(name)
}

// reclaimNetNSDecision returns the slots to keep and the namespaces to tear down.
// An fcns-* name that does not parse is ours by prefix and always stale.
func reclaimNetNSDecision(names []string, live map[string]bool) (keepSlots []int, remove []string) {
	for _, name := range names {
		if !strings.HasPrefix(name, netnsPrefix) {
			continue
		}
		slot, sandboxID, ok := parseNetNSName(name)
		if ok && live[sandboxID] {
			keepSlots = append(keepSlots, slot)
			continue
		}
		remove = append(remove, name)
	}
	return keepSlots, remove
}

var vethHostPattern = regexp.MustCompile(`^vh(\d+)$`)

// reclaimNetNS reconciles named namespaces and host veth ends against live VMMs,
// tearing down leftovers and seeding the slot allocator from the survivors.
// Runs after RecoverOrphans, so a namespace survives only when its VMM did.
func (m *Manager) reclaimNetNS() (removed int) {
	entries, err := os.ReadDir(netnsRunDir)
	if err != nil {
		return 0
	}
	live, ok := liveJailerIDs()
	if !ok {
		// An unreadable /proc is not evidence that nothing is alive: remove
		// nothing, but keep every parseable slot from re-allocation.
		for _, e := range entries {
			if slot, _, pok := parseNetNSName(e.Name()); pok {
				markNetNSSlotUsed(slot)
			}
		}
		m.log.Warn("skipping_netns_reclaim: /proc unreadable; slots reserved, nothing removed")
		return 0
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	keepSlots, remove := reclaimNetNSDecision(names, live)
	for _, s := range keepSlots {
		markNetNSSlotUsed(s)
	}
	for _, name := range remove {
		teardownNetNS(name)
		removed++
	}

	// Host veth ends whose namespace is already gone.
	kept := map[int]bool{}
	for _, s := range keepSlots {
		kept[s] = true
	}
	if links, err := netlink.LinkList(); err == nil {
		for _, l := range links {
			mm := vethHostPattern.FindStringSubmatch(l.Attrs().Name)
			if mm == nil {
				continue
			}
			if slot, err := strconv.Atoi(mm[1]); err == nil && !kept[slot] {
				if netlink.LinkDel(l) == nil {
					removed++
				}
			}
		}
	}
	return removed
}

// detectEgressIf returns the host's default-route interface, or "" when none resolves.
func detectEgressIf() string {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return ""
	}
	for _, r := range routes {
		// The default route is Dst nil in older netlink versions and 0.0.0.0/0 in newer ones.
		isDefault := r.Dst == nil
		if !isDefault && r.Dst != nil {
			if ones, _ := r.Dst.Mask.Size(); ones == 0 {
				isDefault = true
			}
		}
		if isDefault && r.Gw != nil && r.LinkIndex > 0 {
			if link, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
				return link.Attrs().Name
			}
		}
	}
	return ""
}
