package vm

import "testing"

// Under a namespace every sandbox's VMIP is the same fixed guest address, so a
// caller that dials VMIP reaches nothing. HostIP must answer the slot address
// NodeAddr carries, and must keep answering VMIP on the shared bridge.
func TestHostIPPrefersTheReachableNamespaceAddress(t *testing.T) {
	for _, c := range []struct {
		name     string
		sb       Sandbox
		wantHost string
	}{
		{"namespace: the slot address, not the shared guest ip",
			Sandbox{VMIP: "192.168.30.2", NodeAddr: "10.128.4.178:6001"}, "10.128.4.178"},
		{"second sandbox, same guest ip, its own slot",
			Sandbox{VMIP: "192.168.30.2", NodeAddr: "10.128.4.182:6001"}, "10.128.4.182"},
		{"shared bridge: NodeAddr is the guest's own address",
			Sandbox{VMIP: "192.168.30.9", NodeAddr: "192.168.30.9:6001"}, "192.168.30.9"},
		{"no NodeAddr recorded at all falls back to VMIP",
			Sandbox{VMIP: "192.168.30.9"}, "192.168.30.9"},
		{"a NodeAddr with no port is not a host",
			Sandbox{VMIP: "192.168.30.9", NodeAddr: "10.128.4.178"}, "192.168.30.9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sb.HostIP(); got != c.wantHost {
				t.Fatalf("HostIP() = %q, want %q (VMIP %q, NodeAddr %q)",
					got, c.wantHost, c.sb.VMIP, c.sb.NodeAddr)
			}
		})
	}
}
