//go:build linux

package vm

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The guest side pins the same literal; the two packages cannot share a constant.
func TestSetNetWireFormat(t *testing.T) {
	const want = "SETNET 192.168.0.7/16 192.168.30.1 aa:fc:c0:a8:00:07 eth0"
	got := guestNetworkFor(net.IPv4(192, 168, 0, 7)).command()
	if got != want {
		t.Fatalf("SETNET line\n got: %s\nwant: %s", got, want)
	}
}

func TestGuestNetworkMACMatchesTheColdPath(t *testing.T) {
	for _, ip := range []net.IP{
		net.IPv4(192, 168, 0, 7),
		net.IPv4(192, 168, 30, 2),
		net.IPv4(192, 168, 255, 254),
	} {
		g := guestNetworkFor(ip)
		if want := strings.ToLower(generateMAC(ip)); g.MAC != want {
			t.Errorf("guestNetworkFor(%s).MAC = %s, want %s (the MAC the cold path gives Firecracker)",
				ip, g.MAC, want)
		}
		if g.MAC != strings.ToLower(g.MAC) {
			t.Errorf("MAC %s is not lower case; /sys/class/net/eth0/address always is", g.MAC)
		}
		if !g.Gateway.Equal(GatewayIP) {
			t.Errorf("gateway = %s, want %s", g.Gateway, GatewayIP)
		}
	}
}

// The rewrite and the cold-boot cmdline are produced by different code and have drifted before.
func TestGuestNetworkMatchesTheKernelArgs(t *testing.T) {
	ip := net.IPv4(192, 168, 0, 7)
	args := buildKernelArgs(ip, "sb-1-aa", "")
	g := guestNetworkFor(ip)

	if !strings.Contains(args, "ip="+ip.String()+"::"+g.Gateway.String()+":255.255.0.0:sb-1-aa:"+g.Device+":off") {
		t.Fatalf("the cold cmdline (%q) does not describe the same network as SETNET (%s)", args, g.command())
	}
	if ones, _ := g.Mask.Size(); ones != 16 {
		t.Errorf("SETNET prefix is /%d while the cmdline netmask is 255.255.0.0", ones)
	}
}

// A partial rewrite leaves a guest with a new address and the golden MAC.
func TestDeliverGuestNetworkRefusesAnIncompleteIdentity(t *testing.T) {
	full := guestNetworkFor(net.IPv4(192, 168, 0, 7))
	cases := map[string]guestNetwork{
		"no address": {Mask: full.Mask, Gateway: full.Gateway, MAC: full.MAC, Device: full.Device},
		"no gateway": {IP: full.IP, Mask: full.Mask, MAC: full.MAC, Device: full.Device},
		"no mac":     {IP: full.IP, Mask: full.Mask, Gateway: full.Gateway, Device: full.Device},
		"no device":  {IP: full.IP, Mask: full.Mask, Gateway: full.Gateway, MAC: full.MAC},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			// Without the guard this fails anyway (no socket), so assert the guard's own error.
			err := deliverGuestNetwork(t.TempDir(), "sb-test", g, 0)
			if err == nil {
				t.Fatal("an incomplete identity was accepted")
			}
			if !strings.Contains(err.Error(), "incomplete network") {
				t.Fatalf("error %q is not the guard firing", err)
			}
		})
	}
}

func TestSetNetCommandMatchesEnvd(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "envd", "netconfig.go"))
	if err != nil {
		t.Fatalf("read envd's netconfig.go: %v", err)
	}
	if !strings.Contains(string(src), `credSetNet = "`+credSetNetCommand+`"`) {
		t.Fatalf("internal/vm sends %q but cmd/envd/netconfig.go does not define credSetNet as that",
			credSetNetCommand)
	}
}

// The guest sets CLOCK_REALTIME to this number verbatim, so seconds where
// nanoseconds are meant would land it in 1970 and read as a correct push.
func TestDeliverGuestClockSendsNanosecondsFromNow(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := startFakeVMSocketAt(t, filepath.Join(runDir, "sb-1.vsock"),
		fakeVM{connectAck: "OK 1073741824", setHashReply: "OK"})

	before := time.Now()
	if err := deliverGuestClock(root, "sb-1"); err != nil {
		t.Fatalf("deliverGuestClock: %v", err)
	}
	got := srv.received()
	if len(got) != 2 {
		t.Fatalf("guest saw %q, want a CONNECT and a SETTIME", got)
	}
	arg, ok := strings.CutPrefix(got[1], credSetTimeCommand+" ")
	if !ok {
		t.Fatalf("guest saw %q, want a line starting %q", got[1], credSetTimeCommand)
	}
	ns, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		t.Fatalf("SETTIME argument %q is not an integer: %v", arg, err)
	}
	if sent := time.Unix(0, ns); sent.Before(before) || sent.After(time.Now()) {
		t.Fatalf("SETTIME carried %s, want host time between %s and now — the guest "+
			"sets its wall clock to this verbatim", sent, before)
	}
}
