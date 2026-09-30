//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
)

// PreflightIsolation creates a canary tap on fcbr0, isolates it and verifies the
// kernel reports it isolated. Not read-only: it creates and deletes a real interface.
func PreflightIsolation() error {
	tap := buildTuntap("fcpreflight", 0)
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("create canary tap: %w", err)
	}
	defer func() { _ = netlink.LinkDel(tap) }()

	bridge, err := netlink.LinkByName("fcbr0")
	if err != nil {
		return fmt.Errorf("bridge fcbr0 not found (run setup-host.sh): %w", err)
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		return fmt.Errorf("attach canary tap to fcbr0: %w", err)
	}
	if err := netlink.LinkSetIsolated(tap, true); err != nil {
		return fmt.Errorf("isolate canary tap: %w", err)
	}
	pi, err := netlink.LinkGetProtinfo(tap)
	if err != nil {
		return fmt.Errorf("read canary tap protinfo: %w", err)
	}
	if !pi.Isolated {
		return errors.New("kernel did not report canary tap as isolated after LinkSetIsolated")
	}
	return nil
}

// PreflightNetNS builds a canary namespace and reads the kernel's view back to
// confirm the in-ns ruleset, host table and ip_forward are actually loaded. A
// ruleset write that returned nil is not evidence the rules are in the kernel.
func PreflightNetNS() error {
	// nil rules, so no name layer is started and the logger is never used.
	ns, err := setupNetNS("preflight", 0, netnsTapName, net.ParseIP(netnsGuestIP), nil, zap.NewNop())
	if err != nil {
		return fmt.Errorf("canary netns (the exact path every netns create uses): %w", err)
	}
	defer teardownNetNS(ns.Name)

	inNS, err := exec.CommandContext(context.Background(), "nsenter", "--net="+ns.NetNSPath, "nft", "list", "table", "ip", "sandbox").CombinedOutput()
	if err != nil {
		return fmt.Errorf("read canary in-ns ruleset: %w: %s", err, strings.TrimSpace(string(inNS)))
	}
	for _, want := range []struct{ token, meaning string }{
		{"policy drop", "default-deny forward chain (finding A: without it egress is unrestricted)"},
		{"ip daddr 169.254.0.0/16 drop", "metadata/link-local block"},
		{"ip daddr 10.0.0.0/8 drop", "RFC1918 10/8 + inter-slot block"},
		{"ip daddr 172.16.0.0/12 drop", "RFC1918 172.16/12 block"},
		{"ip daddr 100.64.0.0/10 drop", "CGNAT block"},
	} {
		if !strings.Contains(string(inNS), want.token) {
			return fmt.Errorf("canary in-ns ruleset is missing %q — the %s is not loaded", want.token, want.meaning)
		}
	}

	host, err := exec.CommandContext(context.Background(), "nft", "list", "table", "ip", netnsHostTable).CombinedOutput()
	if err != nil {
		return fmt.Errorf("host table %s missing (EnsureNetNSHostRules did not run or did not survive): %w: %s",
			netnsHostTable, err, strings.TrimSpace(string(host)))
	}
	if !strings.Contains(string(host), "masquerade") {
		return fmt.Errorf("host table %s has no masquerade — netns sandboxes would have no egress", netnsHostTable)
	}

	// Without an IPv6 stack there is no packet to filter and the table cannot load.
	if hostHasIPv6() {
		host6, err := exec.CommandContext(context.Background(), "nft", "list", "table", "ip6", netnsHostTable6).CombinedOutput()
		if err != nil {
			return fmt.Errorf("this host has an IPv6 stack (/proc/net/if_inet6) and the ip6 table %s "+
				"is not loaded, so nothing filters IPv6 across sandbox veths — every other rule on this "+
				"node is `table ip` and matches v4 only: %v: %s",
				netnsHostTable6, err, strings.TrimSpace(string(host6)))
		}
		if !strings.Contains(string(host6), `iifname "vh*" drop`) {
			return fmt.Errorf("ip6 table %s is loaded but carries no `iifname \"vh*\" drop` — a guest "+
				"that comes up with IPv6 reaches the host's veth link-local and anything bound on :: "+
				"with no rule in the path", netnsHostTable6)
		}
	}

	fwd, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return fmt.Errorf("read ip_forward: %w", err)
	}
	if strings.TrimSpace(string(fwd)) != "1" {
		return errors.New("ip_forward is 0 — every guest packet is silently dropped between the vh veth and the egress NIC")
	}
	return nil
}

// SeccompEnabled reports whether VMs launch with seccomp on. A static statement
// of intent, since no VM exists when the boot preflight runs.
func SeccompEnabled() bool {
	return true
}
