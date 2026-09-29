//go:build linux

package vm

import (
	"net"
	"testing"
)

// attachTapToBridge pins net.ParseMAC(generateMAC(vmIP)); an unparseable form would fail every boot.
func TestGuestMACPinIsParseable(t *testing.T) {
	for _, ip := range []net.IP{
		net.IPv4(192, 168, 0, 2),
		net.IPv4(192, 168, 0, 7),
		net.IPv4(192, 168, 30, 1),
		net.IPv4(192, 168, 255, 254),
	} {
		mac, err := net.ParseMAC(generateMAC(ip))
		if err != nil {
			t.Fatalf("generateMAC(%s)=%q is not a parseable MAC: %v", ip, generateMAC(ip), err)
		}
		if len(mac) != 6 {
			t.Fatalf("generateMAC(%s) parsed to %d bytes, want 6", ip, len(mac))
		}
		if got, want := mac.String(), guestNetworkFor(ip).MAC; got != want {
			t.Fatalf("pinned MAC for %s = %q, but guest is told %q — the FDB entry would name the wrong address", ip, got, want)
		}
	}
}

func TestBuildTuntap(t *testing.T) {
	t.Run("uid zero leaves owner/group unset", func(t *testing.T) {
		tap := buildTuntap("fctap1", 0)
		if tap.Owner != 0 || tap.Group != 0 {
			t.Fatalf("uid=0 must leave Owner/Group at zero value, got Owner=%d Group=%d", tap.Owner, tap.Group)
		}
		if tap.Name != "fctap1" {
			t.Fatalf("tap name = %q, want fctap1", tap.Name)
		}
	})

	t.Run("nonzero uid sets owner and group", func(t *testing.T) {
		tap := buildTuntap("fctap2", 23417)
		if tap.Owner != 23417 {
			t.Fatalf("Owner = %d, want 23417", tap.Owner)
		}
		if tap.Group != 23417 {
			t.Fatalf("Group = %d, want 23417", tap.Group)
		}
	})
}
