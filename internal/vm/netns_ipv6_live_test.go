//go:build linux

package vm

import (
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Live nftables tests: a refused table is no boundary at all, since `nft -f`
// fails atomically, and only nft decides what is legal. Needs nft and root;
// skips elsewhere. All work happens in throwaway namespaces.

// liveNetNS creates a throwaway namespace for a ruleset probe, or skips.
func liveNetNS(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 not installed")
	}
	ns := "nftprobe-" + strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()))
	_ = exec.Command("ip", "netns", "delete", ns).Run() // a leftover from a killed run
	if out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput(); err != nil {
		t.Skipf("cannot create a test namespace (needs root): %v: %s", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "delete", ns).Run() })
	return ns
}

// loadRuleset feeds a ruleset to nft inside ns and returns nft's own verdict.
func loadRuleset(t *testing.T, ns, ruleset string) (string, error) {
	t.Helper()
	cmd := exec.Command("ip", "netns", "exec", ns, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestLiveIPv6HostRulesetLoads(t *testing.T) {
	ns := liveNetNS(t)
	rs := buildNetNSHostRuleset6()

	out, err := loadRuleset(t, ns, rs)
	if err != nil {
		t.Fatalf("nft REFUSED the IPv6 backstop, so the table would be absent and sandbox "+
			"veths unfiltered over IPv6:\n%s\n\nruleset:\n%s", out, rs)
	}

	listed, err := exec.Command("ip", "netns", "exec", ns, "nft", "list", "table", "ip6", netnsHostTable6).CombinedOutput()
	if err != nil {
		t.Fatalf("the table did not exist after a successful load: %v: %s", err, listed)
	}
	for _, want := range []string{`iifname "vh*"`, "drop"} {
		if !strings.Contains(string(listed), want) {
			t.Errorf("the loaded table does not contain %q — nft accepted the input and kept "+
				"something else:\n%s", want, listed)
		}
	}
}

func TestLiveIPv4RulesetsLoad(t *testing.T) {
	ns := liveNetNS(t)

	for name, rs := range map[string]string{
		"in-namespace": buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false),
		"host":         buildNetNSHostRuleset("eth0"),
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := loadRuleset(t, ns, rs); err != nil {
				t.Fatalf("nft refused the %s ruleset — every sandbox on a node running this "+
					"would have no egress rules at all:\n%s\n\nruleset:\n%s", name, out, rs)
			}
		})
	}
}

func TestLiveBothFamiliesCoexist(t *testing.T) {
	ns := liveNetNS(t)

	if out, err := loadRuleset(t, ns, buildNetNSHostRuleset("eth0")); err != nil {
		t.Fatalf("IPv4 host table: %s", out)
	}
	if out, err := loadRuleset(t, ns, buildNetNSHostRuleset6()); err != nil {
		t.Fatalf("the IPv6 table would not load beside the IPv4 one, which is how a real node "+
			"runs them:\n%s", out)
	}
	for _, tc := range []struct{ family, table string }{
		{"ip", netnsHostTable},
		{"ip6", netnsHostTable6},
	} {
		if err := exec.Command("ip", "netns", "exec", ns, "nft", "list", "table", tc.family, tc.table).Run(); err != nil {
			t.Errorf("table %s %s is missing after loading both: %v", tc.family, tc.table, err)
		}
	}
}

// A table that loads but matches nothing passes every test above, so this one
// has a positive control: the same ping must succeed before the table loads and
// fail after. Topology: [guest] vp0 fd00::2 <--veth--> vh0 fd00::1 [host].
func TestLiveIPv6BackstopActuallyDrops(t *testing.T) {
	hostNS := liveNetNS(t)
	guestNS := hostNS + "-guest"
	_ = exec.Command("ip", "netns", "delete", guestNS).Run()
	if out, err := exec.Command("ip", "netns", "add", guestNS).CombinedOutput(); err != nil {
		t.Skipf("cannot create the second namespace: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "delete", guestNS).Run() })

	// vh0 must keep the vh* prefix the ruleset keys on.
	run := func(args ...string) error { return exec.Command(args[0], args[1:]...).Run() }
	steps := [][]string{
		{"ip", "-n", hostNS, "link", "add", "vh0", "type", "veth", "peer", "name", "vp0"},
		{"ip", "-n", hostNS, "link", "set", "vp0", "netns", guestNS},
		{"ip", "-n", hostNS, "addr", "add", "fd00::1/64", "dev", "vh0"},
		{"ip", "-n", hostNS, "link", "set", "vh0", "up"},
		{"ip", "-n", guestNS, "addr", "add", "fd00::2/64", "dev", "vp0"},
		{"ip", "-n", guestNS, "link", "set", "vp0", "up"},
	}
	for _, s := range steps {
		if err := run(s...); err != nil {
			t.Skipf("could not build the probe topology (%v): %v", s, err)
		}
	}

	ping := func() error {
		return exec.Command("ip", "netns", "exec", guestNS,
			"ping", "-6", "-c", "1", "-W", "2", "fd00::1").Run()
	}

	// Duplicate Address Detection holds a fresh address tentative for a moment.
	deadline := time.Now().Add(5 * time.Second)
	var controlErr error
	for time.Now().Before(deadline) {
		if controlErr = ping(); controlErr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if controlErr != nil {
		t.Skipf("POSITIVE CONTROL FAILED: IPv6 does not traverse the veth even with no rules "+
			"loaded, so this host cannot answer the question: %v", controlErr)
	}

	if out, err := loadRuleset(t, hostNS, buildNetNSHostRuleset6()); err != nil {
		t.Fatalf("loading the backstop: %s", out)
	}

	if err := ping(); err == nil {
		t.Fatal("IPv6 STILL REACHES the host across a vh* veth after the backstop is loaded. " +
			"The table exists and matches nothing — which is exactly the state the unit tests " +
			"and the load tests above are unable to distinguish from a working control.")
	}
}

// A node on a tailnet has a route to 100.64.0.0/10 on the host, so the host's
// forward chain must stop a sandbox using it. The positive controls are the same
// ping before the table loads, and a public address that must still pass after.
// Topology: [guest] vp0 10.128.0.2 <--> vh0 10.128.0.1 [host] vt0 <--> vt1 [far].
func TestLiveHostRulesetStopsASandboxReachingTheTailnetRange(t *testing.T) {
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skip("ping not installed")
	}
	hostNS := liveNetNS(t)
	guestNS, farNS := hostNS+"-guest", hostNS+"-far"
	for _, ns := range []string{guestNS, farNS} {
		_ = exec.Command("ip", "netns", "delete", ns).Run()
		if out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput(); err != nil {
			t.Skipf("cannot create namespace %s: %v: %s", ns, err, out)
		}
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "delete", ns).Run() })
	}
	for _, s := range [][]string{
		{"ip", "-n", hostNS, "link", "add", "vh0", "type", "veth", "peer", "name", "vp0"},
		{"ip", "-n", hostNS, "link", "set", "vp0", "netns", guestNS},
		{"ip", "-n", hostNS, "addr", "add", "10.128.0.1/30", "dev", "vh0"},
		{"ip", "-n", hostNS, "link", "set", "vh0", "up"},
		{"ip", "-n", guestNS, "addr", "add", "10.128.0.2/30", "dev", "vp0"},
		{"ip", "-n", guestNS, "link", "set", "vp0", "up"},
		{"ip", "-n", guestNS, "route", "add", "default", "via", "10.128.0.1"},
		{"ip", "-n", hostNS, "link", "add", "vt0", "type", "veth", "peer", "name", "vt1"},
		{"ip", "-n", hostNS, "link", "set", "vt1", "netns", farNS},
		{"ip", "-n", hostNS, "addr", "add", "100.64.0.1/24", "dev", "vt0"},
		{"ip", "-n", hostNS, "addr", "add", "203.0.113.1/24", "dev", "vt0"},
		{"ip", "-n", hostNS, "link", "set", "vt0", "up"},
		{"ip", "-n", farNS, "addr", "add", "100.64.0.2/24", "dev", "vt1"},
		{"ip", "-n", farNS, "addr", "add", "203.0.113.2/24", "dev", "vt1"},
		{"ip", "-n", farNS, "link", "set", "vt1", "up"},
		{"ip", "-n", farNS, "route", "add", "default", "via", "100.64.0.1"},
		{"ip", "netns", "exec", hostNS, "sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward"},
	} {
		if out, err := exec.Command(s[0], s[1:]...).CombinedOutput(); err != nil {
			t.Skipf("could not build the probe topology (%v): %v: %s", s, err, out)
		}
	}

	const tailnetAddr, publicAddr = "100.64.0.2", "203.0.113.2"
	ping := func(addr string) error {
		return exec.Command("ip", "netns", "exec", guestNS, "ping", "-c", "1", "-W", "2", addr).Run()
	}
	for _, addr := range []string{tailnetAddr, publicAddr} {
		if err := ping(addr); err != nil {
			t.Skipf("POSITIVE CONTROL FAILED: %s is unreachable with no rules loaded, so this host "+
				"cannot answer the question: %v", addr, err)
		}
	}

	if out, err := loadRuleset(t, hostNS, buildNetNSHostRuleset("vt0")); err != nil {
		t.Fatalf("loading the host ruleset: %s", out)
	}

	if err := ping(tailnetAddr); err == nil {
		t.Error("a sandbox still reaches the tailnet range through the host")
	}
	if err := ping(publicAddr); err != nil {
		t.Errorf("the rule also stopped public traffic, which every sandbox needs: %v", err)
	}
}
