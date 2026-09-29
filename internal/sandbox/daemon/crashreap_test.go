package daemon

import (
	"context"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// deadVMM is an instance whose VMM process reports as gone.
type deadVMM struct{ rt.Instance }

func (deadVMM) Alive() bool { return false }

func TestReconcileReapsCrashedVMM(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d.mu.Lock()
	d.boxes[sb.ID].inst = deadVMM{d.boxes[sb.ID].inst}
	d.mu.Unlock()

	d.reconcileOnce(ctx)

	if _, ok := d.Get(sb.ID); ok {
		t.Fatalf("crashed sandbox still present after reconcile — VMM-death not reaped")
	}
}

func TestReconcileKeepsLiveSandbox(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.reconcileOnce(ctx)
	if _, ok := d.Get(sb.ID); !ok {
		t.Fatalf("live sandbox was wrongly reaped")
	}
}
