package vm

import "net"

// HostIP is the address the node can reach this sandbox on.
//
// VMIP is the guest's own address. Under FC_NETNS that is the same fixed
// 192.168.30.2 in every sandbox and routes from nowhere on the host, so
// anything that dials it — the preview proxy, the web terminal, the agent —
// times out. The reachable address is the namespace's per-slot one, which
// NodeAddr carries and the in-namespace DNAT fronts for every port.
//
// On the shared bridge NodeAddr is VMIP's own address, so this is the same
// answer either way and callers need no mode of their own.
func (s *Sandbox) HostIP() string {
	if h, _, err := net.SplitHostPort(s.NodeAddr); err == nil && h != "" {
		return h
	}
	return s.VMIP
}
