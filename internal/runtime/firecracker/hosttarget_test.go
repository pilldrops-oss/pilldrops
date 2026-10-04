package firecracker

import (
	"context"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Everything the node dials — the preview proxy, the web terminal — must go
// to the address the node can reach, not to the guest's own IP. Under
// FC_NETNS that guest IP is the same 192.168.30.2 in every sandbox and routes
// from nowhere on the host, which is what made previews 502.
//
// The three paths that build a sandbox are covered because each rebuilds the
// namespace and so re-derives the address: cold boot, restore and fork.
func TestTargetsUseTheNodeReachableAddress(t *testing.T) {
	t.Setenv("FC_MEM_SNAPSHOTS", "1")

	for _, mode := range []struct {
		name     string
		nodeAddr string // what the vm layer recorded
		wantBoot string // host the three targets must use after a cold boot
		wantRest string // ... after a restore
		wantFork string // ... after a fork
	}{
		{"shared bridge: NodeAddr is the guest's own address",
			"", "192.168.30.7", "192.168.30.9", "192.168.30.2"},
		{"namespace: the slot address the DNAT fronts, never the fixed guest ip",
			"10.128.4.178:6001", "10.128.4.178", "10.128.4.178", "10.128.4.178"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			be := newFake()
			be.nodeAddr = mode.nodeAddr
			r := newWithBackend(be)

			boot, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "base"}})
			if err != nil {
				t.Fatalf("boot: %v", err)
			}
			restored, err := r.Restore(context.Background(), rt.SnapshotRef{Path: "/snap/sb1"},
				rt.BootSpec{SandboxID: "sb1", Template: rt.Template{Name: "base"}})
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			forked, _, err := r.Fork(context.Background(), rt.ForkSource{Parent: boot},
				rt.BootSpec{SandboxID: "sb2", Template: rt.Template{Name: "base"}})
			if err != nil {
				t.Fatalf("fork: %v", err)
			}

			for _, c := range []struct {
				path string
				inst rt.Instance
				want string
			}{
				{"cold boot", boot, mode.wantBoot},
				{"restore", restored, mode.wantRest},
				{"fork", forked, mode.wantFork},
			} {
				// The port is always the caller's; only the host changes.
				if got, err := c.inst.ListenTarget(8080); err != nil || got != c.want+":8080" {
					t.Errorf("%s ListenTarget(8080) = %q, %v; want %q", c.path, got, err, c.want+":8080")
				}
				if got, err := c.inst.ListenTarget(0); err != nil || got != c.want+":8000" {
					t.Errorf("%s ListenTarget(0) = %q, %v; want the app port on %q", c.path, got, err, c.want)
				}
				term, err := c.inst.TerminalTarget()
				if err != nil || term != c.want+":7681" {
					t.Errorf("%s TerminalTarget = %q, %v; want %q", c.path, term, err, c.want+":7681")
				}
			}
		})
	}
}
