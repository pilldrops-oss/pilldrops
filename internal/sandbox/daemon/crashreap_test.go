package daemon

import (
	"context"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// deadVMM is an instance whose VMM process reports as gone.
type deadVMM struct{ rt.Instance }

func (deadVMM) Alive() bool { return false }

// oomVMM is a dead instance its runtime saw killed for memory.
type oomVMM struct{ deadVMM }

func (oomVMM) OOMKilled() bool { return true }

func TestScanReapsADeadInstanceAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(rt.Instance) rt.Instance
		want string
	}{
		{"exited", func(i rt.Instance) rt.Instance { return deadVMM{i} }, "exited"},
		{"oom", func(i rt.Instance) rt.Instance { return oomVMM{deadVMM{i}} }, "oom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePersister(nil)
			d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
			ctx := context.Background()
			sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			d.mu.Lock()
			d.boxes[sb.ID].inst = tc.wrap(d.boxes[sb.ID].inst)
			d.mu.Unlock()

			d.scanExits(ctx)

			if _, ok := d.Get(sb.ID); ok {
				t.Fatal("a sandbox whose instance died still lists as running")
			}
			if n := fp.countEvents(tc.want); n != 1 {
				t.Errorf("%q events = %d, want 1 (events %v)", tc.want, n, fp.events)
			}
			if got := fp.history[sb.ID].ExitState; got != tc.want {
				t.Errorf("history exit state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanKeepsLiveSandbox(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.scanExits(ctx)
	d.reconcileOnce(ctx)
	if _, ok := d.Get(sb.ID); !ok {
		t.Fatalf("live sandbox was wrongly reaped")
	}
}
