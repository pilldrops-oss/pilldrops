//go:build linux

package vm

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var ipCounter atomic.Uint32

func init() {
	ipCounter.Store(2) // start at 192.168.30.2; .30.1 is gateway
}

// allocateVM returns a unique IP and tap name from one monotonic counter. Not
// derived from the sandbox id, whose timestamp prefix collides across sandboxes.
func allocateVM() (net.IP, string) {
	for {
		n := ipCounter.Add(1)
		ip := net.IPv4(192, 168, byte(n>>8), byte(n&0xFF))
		tapName := tapNameFor(n)
		if !tapReserved(tapName) {
			return ip, tapName
		}
	}
}

func tapNameFor(n uint32) string {
	return fmt.Sprintf("fctap%d", n) // <=15 chars (IFNAMSIZ) up to fctap65533
}

// reservedTaps holds tap names baked into golden snapshots. Firecracker opens the
// baked name on restore, and the in-memory counter resets on restart, so without
// this the first sandbox after a restart would take the golden's name.
var (
	reservedTapsMu sync.Mutex
	reservedTaps   = map[string]bool{}
)

// ReserveGoldenTap marks name as belonging to a golden snapshot.
func ReserveGoldenTap(name string) {
	if name == "" {
		return
	}
	reservedTapsMu.Lock()
	reservedTaps[name] = true
	reservedTapsMu.Unlock()
}

func tapReserved(name string) bool {
	reservedTapsMu.Lock()
	defer reservedTapsMu.Unlock()
	return reservedTaps[name]
}

// goldenTapClaims serialises the window in which a restore holds a golden
// snapshot's baked device name. Keyed by name, not template, because two
// templates can legitimately have baked the same name.
var (
	goldenTapClaimsMu sync.Mutex
	goldenTapClaims   = map[string]*sync.Mutex{}
)

// claimGoldenTap blocks until this process holds name, and returns the release.
func claimGoldenTap(name string) func() {
	goldenTapClaimsMu.Lock()
	mu, ok := goldenTapClaims[name]
	if !ok {
		mu = &sync.Mutex{}
		goldenTapClaims[name] = mu
	}
	goldenTapClaimsMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

type NetworkSetup struct {
	TapName   string
	VMIP      net.IP
	GatewayIP net.IP
}

// SetupVMNetwork allocates an address and bridges a tap owned by uid (0 for root).
func SetupVMNetwork(sandboxID string, uid int) (*NetworkSetup, error) {
	vmIP, tapName := allocateVM()
	return bridgeTap(tapName, vmIP, uid)
}

// SetupVMNetworkWithIP bridges a fresh tap for a guest IP a snapshot already
// holds; only the tap name is allocated.
func SetupVMNetworkWithIP(vmIP net.IP, uid int) (*NetworkSetup, error) {
	_, tapName := allocateVM()
	return bridgeTap(tapName, vmIP, uid)
}

// buildTuntap assembles the tap request. A non-zero uid sets Owner and Group,
// or the non-root VMM cannot open a tap implicitly owned by uid 0.
func buildTuntap(tapName string, uid int) *netlink.Tuntap {
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: tapName},
		Mode:      netlink.TUNTAP_MODE_TAP,
	}
	if uid != 0 {
		tap.Owner = uint32(uid)
		tap.Group = uint32(uid)
	}
	return tap
}

// GatewayIP is fcbr0's own address and the guest's default route.
var GatewayIP = net.IPv4(192, 168, 30, 1)

// createTap creates and brings up a tap without bridging it. A restored guest
// still holds the golden identity until SETNET, and must not be on fcbr0 until then.
func createTap(tapName string, uid int) (netlink.Link, error) {
	tap := buildTuntap(tapName, uid)
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, fmt.Errorf("create TAP %s: %w", tapName, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		_ = netlink.LinkDel(tap)
		return nil, fmt.Errorf("bring TAP %s up: %w", tapName, err)
	}
	return tap, nil
}

// attachTapToBridge is the only place a tap becomes an fcbr0 port. Isolation
// blocks port-to-port frames; the permanent FDB pin plus learning off stops a
// guest spoofing a neighbour's source MAC to steal host-to-guest traffic.
func attachTapToBridge(tap netlink.Link, guestMAC net.HardwareAddr) error {
	bridge, err := netlink.LinkByName("fcbr0")
	if err != nil {
		return fmt.Errorf("bridge not found (run setup-host.sh): %w", err)
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		return fmt.Errorf("attach TAP %s to fcbr0: %w", tap.Attrs().Name, err)
	}
	// Learning off before the tap is up, so no spoofed frame can ever move an FDB entry.
	if err := netlink.LinkSetLearning(tap, false); err != nil {
		return fmt.Errorf("disable MAC learning on TAP %s: %w", tap.Attrs().Name, err)
	}
	if err := pinGuestMAC(tap, guestMAC); err != nil {
		return fmt.Errorf("pin MAC %s to TAP %s: %w", guestMAC, tap.Attrs().Name, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("bring TAP %s up: %w", tap.Attrs().Name, err)
	}
	if err := netlink.LinkSetIsolated(tap, true); err != nil {
		return fmt.Errorf("isolate TAP %s: %w", tap.Attrs().Name, err)
	}
	return nil
}

// pinGuestMAC is `bridge fdb replace <mac> dev <tap> master static`. Replace, not
// add, so a stale entry from a crashed sandbox that reused this MAC is overwritten.
func pinGuestMAC(tap netlink.Link, guestMAC net.HardwareAddr) error {
	return netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    tap.Attrs().Index,
		Family:       unix.AF_BRIDGE,
		State:        netlink.NUD_NOARP | netlink.NUD_PERMANENT,
		Flags:        netlink.NTF_MASTER,
		HardwareAddr: guestMAC,
	})
}

// renameTap moves a tap off the golden snapshot's baked name once the VMM has
// opened it. The rename works while the VMM holds the tap open and up.
func renameTap(oldName, newName string) (netlink.Link, error) {
	link, err := netlink.LinkByName(oldName)
	if err != nil {
		return nil, fmt.Errorf("TAP %s vanished before it could be renamed: %w", oldName, err)
	}
	if err := netlink.LinkSetName(link, newName); err != nil {
		return nil, fmt.Errorf("rename TAP %s to %s: %w", oldName, newName, err)
	}
	// The cached Link still carries the old name.
	link, err = netlink.LinkByName(newName)
	if err != nil {
		return nil, fmt.Errorf("TAP %s not found after rename from %s: %w", newName, oldName, err)
	}
	return link, nil
}

// bridgeTap creates tapName, attaches it to fcbr0 and returns the NetworkSetup bound to vmIP.
func bridgeTap(tapName string, vmIP net.IP, uid int) (*NetworkSetup, error) {
	tap, err := createTap(tapName, uid)
	if err != nil {
		return nil, err
	}
	mac, err := net.ParseMAC(generateMAC(vmIP))
	if err != nil {
		_ = netlink.LinkDel(tap)
		return nil, fmt.Errorf("guest MAC for %s: %w", vmIP, err)
	}
	if err := attachTapToBridge(tap, mac); err != nil {
		_ = netlink.LinkDel(tap)
		return nil, err
	}
	return &NetworkSetup{TapName: tapName, VMIP: vmIP, GatewayIP: GatewayIP}, nil
}

func TeardownVMNetwork(tapName string) {
	if link, err := netlink.LinkByName(tapName); err == nil {
		_ = netlink.LinkDel(link)
	}
}

// cleanupStaleTaps advances the in-memory tap counter past every fctapN on the
// host, whoever owns it, and deletes the taps this daemon recorded for sandboxes
// no longer active. Unrecorded taps may be a co-hosted daemon's and are left alone.
func (m *Manager) cleanupStaleTaps() int {
	links, err := netlink.LinkList()
	if err != nil {
		return 0
	}
	maxN := uint32(2)
	for _, l := range links {
		name := l.Attrs().Name
		if !strings.HasPrefix(name, "fctap") {
			continue
		}
		if n, err := strconv.ParseUint(name[len("fctap"):], 10, 32); err == nil && uint32(n) > maxN {
			maxN = uint32(n)
		}
	}

	m.mu.RLock()
	active := make(map[string]bool, len(m.active))
	for id := range m.recoverable { // held for adoption: its tap is in use
		active[id] = true
	}
	for id := range m.active {
		active[id] = true
	}
	m.mu.RUnlock()

	deleted := 0
	for _, victim := range staleTapDecision(m.tapRecords(), active) {
		if link, err := netlink.LinkByName(victim.tap); err == nil {
			if netlink.LinkDel(link) == nil {
				deleted++
			}
		}
		m.removeTap(victim.id)
	}

	for {
		cur := ipCounter.Load()
		if cur > maxN {
			break
		}
		if ipCounter.CompareAndSwap(cur, maxN+1) {
			break
		}
	}
	return deleted
}

type staleTap struct{ id, tap string }

// staleTapDecision returns the recorded taps whose sandbox is not active. The
// input is the record set, not the host's device list, so an unrecorded tap is never a victim.
func staleTapDecision(records map[string]string, active map[string]bool) []staleTap {
	var out []staleTap
	for id, tap := range records {
		if active[id] {
			continue
		}
		out = append(out, staleTap{id: id, tap: tap})
	}
	return out
}

func tapExists(name string) bool {
	if name == "" {
		return false
	}
	_, err := netlink.LinkByName(name)
	return err == nil
}
