//go:build linux

package main

// Network self-heal: a snapshot resume flaps the virtio-net link and drops the
// static IPv4 from the kernel ip= arg, and the guest has no network manager or
// ip binary. A loop re-asserts the address and route via netlink; SETNET
// replaces the target because a restored guest's cmdline is the golden VM's.

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
)

// healTarget is the identity healNetwork re-asserts; SETNET replaces it while the loop runs.
var healTarget atomic.Pointer[netConfig]

// startNetHeal starts the heal loop if the guest booted with a static ip= kernel arg.
func startNetHeal() {
	ip, mask, gw, dev := parseKernelIPArg()
	if ip == nil || dev == "" {
		return
	}
	if mask == nil {
		mask = net.CIDRMask(16, 32) // fc bridge is a /16
	}
	healTarget.Store(&netConfig{
		Addr:    &net.IPNet{IP: ip, Mask: mask},
		Gateway: gw,
		Device:  dev,
		// No MAC: the loop must not touch the cold-boot MAC. Only SETNET sets one.
	})
	go func() {
		for {
			if c := healTarget.Load(); c != nil {
				healNetwork(*c)
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

// parseKernelIPArg reads ip=<client-ip>::<gw-ip>:<netmask>::<device>:<autoconf> from /proc/cmdline.
func parseKernelIPArg() (ip net.IP, mask net.IPMask, gw net.IP, dev string) {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, nil, nil, ""
	}
	for _, tok := range strings.Fields(string(b)) {
		if !strings.HasPrefix(tok, "ip=") {
			continue
		}
		f := strings.Split(strings.TrimPrefix(tok, "ip="), ":")
		if len(f) < 6 {
			return nil, nil, nil, ""
		}
		ip = net.ParseIP(f[0])
		gw = net.ParseIP(f[2])
		if m := net.ParseIP(f[3]); m != nil {
			if v4 := m.To4(); v4 != nil {
				mask = net.IPMask(v4)
			}
		}
		dev = f[5]
		return ip, mask, gw, dev
	}
	return nil, nil, nil, ""
}

// healNetwork idempotently re-asserts the address, up-state, and default route.
// It never removes addresses; a pruning loop would race legitimate additions.
func healNetwork(c netConfig) {
	link, err := netlink.LinkByName(c.Device)
	if err != nil {
		return
	}
	_ = netlink.AddrReplace(link, &netlink.Addr{IPNet: c.Addr})
	_ = netlink.LinkSetUp(link)
	if c.Gateway != nil {
		_ = netlink.RouteReplace(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Gw:        c.Gateway,
			Dst:       nil, // default route
		})
	}
}

// applyGuestNetwork installs c on the guest and makes it the heal loop's target.
// The link goes down first because virtio_net lacks IFF_LIVE_ADDR_CHANGE, so
// the kernel refuses a MAC change on a running interface; safe because this
// call arrives over vsock, not eth0.
func applyGuestNetwork(c netConfig) error {
	link, err := netlink.LinkByName(c.Device)
	if err != nil {
		return fmt.Errorf("no interface %s: %w", c.Device, err)
	}

	if err := netlink.LinkSetDown(link); err != nil {
		return fmt.Errorf("set %s down: %w", c.Device, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, c.MAC); err != nil {
		return fmt.Errorf("set %s mac to %s: %w", c.Device, c.MAC, err)
	}

	// AddrReplace alone would add, leaving the golden address answering on the bridge.
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list %s addresses: %w", c.Device, err)
	}
	for i := range addrs {
		if addrs[i].IPNet != nil && addrs[i].IP.Equal(c.Addr.IP) {
			continue // AddrReplace below fixes the mask
		}
		if err := netlink.AddrDel(link, &addrs[i]); err != nil {
			return fmt.Errorf("remove stale address %s from %s: %w", addrs[i].IPNet, c.Device, err)
		}
	}
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: c.Addr}); err != nil {
		return fmt.Errorf("set %s address to %s: %w", c.Device, c.Addr, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set %s up: %w", c.Device, err)
	}
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        c.Gateway,
		Dst:       nil,
	}); err != nil {
		return fmt.Errorf("set default route via %s: %w", c.Gateway, err)
	}

	healTarget.Store(&c)
	return nil
}
