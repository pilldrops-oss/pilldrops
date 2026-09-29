package vm

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// localAPIEgressRule is the one hole in a slot's egress chain when a local
// control plane is configured: the slot's own host veth address, this port,
// TCP. "" otherwise, so the ruleset is byte-identical to today's.
func localAPIEgressRule(hostIP net.IP) string {
	port := localAPIPort()
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("\t\tip daddr %s tcp dport %d accept\n", hostIP, port)
}

// localAPIHostRule is the matching hole in the host input chain, which
// otherwise drops every new connection a sandbox opens to the node.
func localAPIHostRule() string {
	port := localAPIPort()
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("\t\tiifname \"vh*\" ip saddr 10.128.0.0/16 tcp dport %d accept\n", port)
}

// netnsSlotBase is FC_NETNS_SLOT_BASE: the first slot this daemon hands out.
// Slot numbers name host links (vh<slot>) and 10.128/16 addresses, so two
// FC_NETNS daemons on one host (side-cars on a shared test node) must not
// both start at 0: one's teardown deletes the other's veth by name.
// ponytail: a base, not a lock; co-hosting is a test-node arrangement.
func netnsSlotBase() int {
	n, err := strconv.Atoi(os.Getenv("FC_NETNS_SLOT_BASE"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// localAPIPort is PILLDROPS_LOCAL_API_PORT: a control plane on this host that
// guests may reach on exactly this port at their slot's host veth address.
// Unset (0) in production, where the guest reaches the portal on 443 like any
// client and the host stays fully closed. Not in netns_linux.go so the rule
// it drives is asserted on a macOS checkout too.
func localAPIPort() int {
	n, err := strconv.Atoi(os.Getenv("PILLDROPS_LOCAL_API_PORT"))
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}
