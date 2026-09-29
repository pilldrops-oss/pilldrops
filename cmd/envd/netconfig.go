package main

// Guest network identity and the SETNET command a node uses to replace it
// after a snapshot restore, since every guest restored from one golden
// snapshot comes up with the same address and MAC. It travels the vsock
// credential channel because eth0 is exactly what is misconfigured.

import (
	"fmt"
	"net"
	"strings"
)

// credSetNet is the channel command `SETNET <cidr> <gateway> <mac> <device>`.
// All four fields are mandatory; a partial rewrite leaves the golden MAC colliding on the bridge.
const credSetNet = "SETNET"

// netConfig is a validated guest network identity.
type netConfig struct {
	Addr    *net.IPNet
	Gateway net.IP
	MAC     net.HardwareAddr
	Device  string // e.g. eth0
}

func (c netConfig) String() string {
	return fmt.Sprintf("%s %s gw %s mac %s", c.Device, c.Addr, c.Gateway, c.MAC)
}

// maxIfaceName is IFNAMSIZ-1.
const maxIfaceName = 15

// parseNetConfig validates one SETNET argument list. IPv4 only: the bridge is
// a v4 /16, and a v6 target would be re-asserted forever by the heal loop.
func parseNetConfig(fields []string) (netConfig, error) {
	var c netConfig
	if len(fields) != 4 {
		return c, fmt.Errorf("%s takes exactly four arguments (<cidr> <gateway> <mac> <device>), got %d",
			credSetNet, len(fields))
	}

	ip, ipNet, err := net.ParseCIDR(fields[0])
	if err != nil {
		return c, fmt.Errorf("argument 1 (%q) is not a CIDR address", fields[0])
	}
	if ip.To4() == nil {
		return c, fmt.Errorf("argument 1 (%q) is not IPv4", fields[0])
	}
	// ipNet holds the masked network; the interface needs the host address with that mask.
	c.Addr = &net.IPNet{IP: ip.To4(), Mask: ipNet.Mask}

	gw := net.ParseIP(fields[1])
	if gw == nil || gw.To4() == nil {
		return c, fmt.Errorf("argument 2 (%q) is not an IPv4 gateway", fields[1])
	}
	c.Gateway = gw.To4()

	mac, err := net.ParseMAC(fields[2])
	if err != nil {
		return c, fmt.Errorf("argument 3 (%q) is not a MAC address", fields[2])
	}
	if len(mac) != 6 {
		return c, fmt.Errorf("argument 3 (%q) is not a 6-byte MAC address", fields[2])
	}
	c.MAC = mac

	dev := fields[3]
	if dev == "" || len(dev) > maxIfaceName || strings.ContainsAny(dev, " /\t") || dev == "." || dev == ".." {
		return c, fmt.Errorf("argument 4 (%q) is not an interface name", fields[3])
	}
	c.Device = dev

	return c, nil
}

// applyNetConfig installs a validated identity on the guest and replaces the
// heal loop's target, or the loop reverts eth0 to the golden cmdline address
// within one tick. A var so tests can observe what the parser handed over.
var applyNetConfig = applyGuestNetwork
