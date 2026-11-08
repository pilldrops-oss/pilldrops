package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// A fork is a copy: same org, same size, same disk, and the parent keeps running.
func TestForkRunningParentCopiesAndParentKeepsRunning(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	parent, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "acme", VCPU: 2, MemMiB: 2048, Env: []string{"K=v"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pfs, err := d.FS(parent.ID)
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	if err := pfs.Write(ctx, "marker.txt", []byte("from-parent")); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := d.Fork(ctx, parent.ID, ForkSpec{Name: "copy"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	child := res.Child
	if child.ID == parent.ID || child.ID == "" {
		t.Fatalf("child id %q must be new", child.ID)
	}
	if child.State != StateRunning || child.OrgID != "acme" || child.Name != "copy" ||
		child.VCPU != 2 || child.MemMiB != 2048 || child.Template != "python-3.11" {
		t.Fatalf("child = %+v, want running copy of the parent in org acme", child)
	}
	cfs, err := d.FS(child.ID)
	if err != nil {
		t.Fatalf("child fs: %v", err)
	}
	if got, err := cfs.Read(ctx, "marker.txt"); err != nil || string(got) != "from-parent" {
		t.Fatalf("child disk = %q, %v; want the parent's file", got, err)
	}
	if res.ParentID != parent.ID || res.ParentState != StateRunning || res.ParentPaused <= 0 {
		t.Fatalf("result = %+v, want parent id/state and a paused duration", res)
	}
	p, _ := d.Get(parent.ID)
	if p.State != StateRunning {
		t.Fatalf("parent state after fork = %s, want running", p.State)
	}
	// Independent from here: destroying the child leaves the parent.
	if err := d.Destroy(ctx, child.ID); err != nil {
		t.Fatalf("destroy child: %v", err)
	}
	if _, err := d.Exec(ctx, parent.ID, rt.ExecSpec{Cmd: "true"}); err != nil {
		t.Fatalf("parent exec after child destroy: %v", err)
	}
}

func TestForkPausedParentStaysPaused(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	parent, _ := d.Create(ctx, CreateSpec{Template: "python-3.11"})
	if err := d.Pause(ctx, parent.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	res, err := d.Fork(ctx, parent.ID, ForkSpec{})
	if err != nil {
		t.Fatalf("fork paused: %v", err)
	}
	if res.Child.State != StateRunning || res.ParentState != StatePaused {
		t.Fatalf("child %s / parent %s, want running / paused", res.Child.State, res.ParentState)
	}
	p, _ := d.Get(parent.ID)
	if p.State != StatePaused {
		t.Fatalf("parent after fork = %s, want still paused", p.State)
	}
}

func TestForkChildTTLAndUnknownParent(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	if _, err := d.Fork(ctx, "nope", ForkSpec{}); !errors.Is(err, rt.ErrNotFound) {
		t.Fatalf("fork unknown = %v, want ErrNotFound", err)
	}
	parent, _ := d.Create(ctx, CreateSpec{Template: "python-3.11"})
	res, err := d.Fork(ctx, parent.ID, ForkSpec{Timeout: -1})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !res.Child.ExpiresAt.IsZero() {
		t.Fatalf("persistent child has ExpiresAt %v, want zero", res.Child.ExpiresAt)
	}
	res2, _ := d.Fork(ctx, parent.ID, ForkSpec{Timeout: 10 * time.Minute})
	if res2.Child.ExpiresAt.IsZero() {
		t.Fatal("timed child has no ExpiresAt")
	}
}

// A fork belongs to the forker and is private unless the fork asked for "org",
// even when the parent is visible to the whole org: the child is a copy of
// somebody else's machine, with their files still in it.
func TestForkOwnershipAndVisibility(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	parent, err := d.Create(ctx, CreateSpec{
		Template: "python-3.11", OrgID: "acme",
		CreatedBy: "key_alice", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := d.Fork(ctx, parent.ID, ForkSpec{CreatedBy: "key_bob"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if res.Child.CreatedBy != "key_bob" || res.Child.Visibility != "private" {
		t.Errorf("child = (created_by=%q, visibility=%q), want (key_bob, private)",
			res.Child.CreatedBy, res.Child.Visibility)
	}
	// The parent is untouched: visibility is not contagious in either direction.
	if p, _ := d.Get(parent.ID); p.Visibility != "org" || p.CreatedBy != "key_alice" {
		t.Errorf("parent = (created_by=%q, visibility=%q), want (key_alice, org)", p.CreatedBy, p.Visibility)
	}

	shared, err := d.Fork(ctx, parent.ID, ForkSpec{CreatedBy: "key_bob", Visibility: "org"})
	if err != nil {
		t.Fatalf("shared fork: %v", err)
	}
	if shared.Child.Visibility != "org" {
		t.Errorf("fork asked for org = %q", shared.Child.Visibility)
	}

	// The portal forks for an org without naming a key. A private child with
	// no creator was visible to nobody: running, billed, and impossible to
	// read, exec in or destroy.
	unowned, err := d.Fork(ctx, parent.ID, ForkSpec{})
	if err != nil {
		t.Fatalf("unowned fork: %v", err)
	}
	if unowned.Child.Visibility != "org" ||
		!sandbox.Visible(unowned.Child.ID, unowned.Child.Visibility, unowned.Child.CreatedBy, "", false) {
		t.Errorf("fork by an unnamed caller = (created_by=%q, visibility=%q), want org-visible",
			unowned.Child.CreatedBy, unowned.Child.Visibility)
	}
}
