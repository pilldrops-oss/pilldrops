//go:build linux

package vm

import (
	"net"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

// /proc/cmdline is world-readable inside the guest, so what stays off it is the security property.

func testVMIP() net.IP { return net.IPv4(192, 168, 30, 7) }

// The guest gets only the hash and can verify but not forge.
func TestKernelArgsCarryTheHashNeverTheToken(t *testing.T) {
	token, hash := sbtoken.Mint()
	if token == "" || hash == "" || token == hash {
		t.Fatalf("fixture: Mint gave token=%q hash=%q", token, hash)
	}

	args := buildKernelArgs(testVMIP(), "sb-1-aa", hash)

	if !strings.Contains(args, "envd.token_hash="+hash) {
		t.Errorf("cmdline does not carry the hash — a COLD-BOOTED guest would find nothing in\n"+
			"/proc/cmdline and would come up with no credential at all:\n  %s", args)
	}
	if strings.Contains(args, token) {
		t.Errorf("THE TOKEN IS ON THE KERNEL CMDLINE. /proc/cmdline is world-readable in the\n"+
			"guest, so the sandbox can read the credential that authenticates the node to it:\n  %s", args)
	}
}

// An empty envd.token_hash= in a boot log would read as provisioned while the guest fails closed.
func TestKernelArgsOmitAnEmptyHashEntirely(t *testing.T) {
	args := buildKernelArgs(testVMIP(), "sb-1-aa", "")
	if strings.Contains(args, "envd.token_hash") {
		t.Errorf("empty hash produced a token_hash fragment: %s", args)
	}
}

// After init= the kernel leaves the hash alone and it reaches /proc/cmdline verbatim.
func TestKernelArgsPlaceTheHashAfterInit(t *testing.T) {
	args := buildKernelArgs(testVMIP(), "sb-1-aa", "deadbeef")
	init := strings.Index(args, "init=")
	hash := strings.Index(args, "envd.token_hash=")
	if init < 0 || hash < 0 {
		t.Fatalf("cmdline is missing init= or the hash: %s", args)
	}
	if hash < init {
		t.Errorf("the hash is BEFORE init=, where the kernel may consume it: %s", args)
	}
}

// The /16 netmask is what the VM-to-VM drop rule is written against.
func TestKernelArgsBootTheGuestNetwork(t *testing.T) {
	args := buildKernelArgs(net.IPv4(192, 168, 30, 7), "sb-1-aa", "")
	for _, want := range []string{
		"ip=192.168.30.7::192.168.30.1:255.255.0.0:sb-1-aa:eth0:off", // addr, gw, /16 mask, hostname
		"init=/sbin/overlay-init",                                    // per-sandbox overlay root
		"console=ttyS0",                                              // boot diagnostics
	} {
		if !strings.Contains(args, want) {
			t.Errorf("cmdline is missing %q:\n  %s", want, args)
		}
	}
}

// The fifth ip= field is the hostname. Empty, the kernel names the guest after
// its own address — and under FC_NETNS every sandbox holds the same fixed
// 192.168.30.2, so every sandbox on the node would answer `hostname` with it.
// Measured live before this field was filled in.
func TestKernelArgsNameTheGuestAfterItsSandbox(t *testing.T) {
	args := buildKernelArgs(net.IPv4(192, 168, 30, 2), "sb-1790014206223-70f71838", "")
	if !strings.Contains(args, ":sb-1790014206223-70f71838:eth0:") {
		t.Errorf("the ip= hostname field does not hold the sandbox id, so the guest is named\n"+
			"after 192.168.30.2 — the same name every other sandbox on an FC_NETNS node gets:\n  %s", args)
	}
}

// Guards the assumption buildKernelArgs is built on, not buildKernelArgs itself.
func TestEachSandboxGetsItsOwnHash(t *testing.T) {
	seen := map[string]bool{}
	for i := range 64 {
		_, hash := sbtoken.Mint()
		if seen[hash] {
			t.Fatalf("Mint repeated a hash after %d draws — sandboxes would share a credential", i)
		}
		seen[hash] = true
	}
}

// Every nftables rule is ip-family only, so this is the sole control on IPv6.
// Relax it only once ip6/inet tables cover the same ground.
func TestKernelArgsDisableIPv6(t *testing.T) {
	args := buildKernelArgs(testVMIP(), "sb-1-aa", "")
	if !strings.Contains(args, "ipv6.disable=1") {
		t.Fatalf("guest boots with an IPv6 stack and NO rule filters it — the nft rules are\n"+
			"all `table ip` (v4 only), so fe80:: to the host veth and anything the node\n"+
			"bound on :: is reachable with nothing in the way:\n  %s", args)
	}
	// Before init=, or the kernel never parses it.
	if strings.Index(args, "ipv6.disable=1") > strings.Index(args, "init=") {
		t.Errorf("ipv6.disable=1 is after init= — the kernel does not parse it there,\n"+
			"so the guest keeps IPv6 while the cmdline claims otherwise:\n  %s", args)
	}
}
