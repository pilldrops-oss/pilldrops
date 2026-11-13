package daemon

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/pilldrops-oss/pilldrops/internal/observability"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// ForkSpec is what differs between a child and its parent: everything else
// (template, size, env, start command, idle policy) is inherited.
type ForkSpec struct {
	Name string
	// Timeout is the child's hard TTL; 0 = Config.HardTTL, <0 = persistent.
	Timeout time.Duration
	// CreatedBy is the key that forked, which owns the child however the
	// parent was owned. Visibility is the child's own: "private" unless the
	// forker asked for "org". A fork is a copy of somebody else's machine with
	// their files in it, so it does not inherit the parent's org visibility.
	CreatedBy  string
	Visibility string
}

// ForkResult is the child plus what the copy cost.
type ForkResult struct {
	Child        SandboxInfo
	ParentID     string
	ParentState  State
	ParentPaused time.Duration
	BootMs       int64
}

// Fork copies parentID, memory, disk and running processes, into a new
// sandbox in the same org. A running parent stands still for the copy and
// carries on; a paused parent is copied from its snapshot and stays paused.
// Serialized with pause/resume/destroy of the parent on its opMu.
func (d *Daemon) Fork(ctx context.Context, parentID string, spec ForkSpec) (res ForkResult, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Fork", attribute.String("parent_id", parentID))
	defer func() { observability.End(span, err) }()

	if !d.isolationOK.Load() {
		return ForkResult{}, ErrIsolationPreflight
	}
	if err := d.refuseIfVolumes(ctx, parentID); err != nil {
		return ForkResult{}, err
	}
	forker, ok := d.cfg.Runtime.(rt.Forker)
	if !ok {
		return ForkResult{}, fmt.Errorf("%w: the %s runtime cannot fork", rt.ErrUnsupported, d.cfg.Runtime.Name())
	}
	parent, release, err := d.acquireOp(parentID)
	if err != nil {
		return ForkResult{}, err
	}
	defer release()

	d.mu.Lock()
	var src rt.ForkSource
	switch parent.State {
	case StateRunning:
		src.Parent = parent.inst
	case StatePaused:
		src.Snapshot = parent.Snapshot
	default:
		st := parent.State
		d.mu.Unlock()
		return ForkResult{}, fmt.Errorf("cannot fork from state %s", st)
	}
	template, orgID := parent.Template, parent.OrgID
	image, ports := parent.Image, parent.Ports
	vcpu, memMiB, storageMB := parent.VCPU, parent.MemMiB, parent.StorageMB
	startCmd, idle := parent.StartCmd, parent.IdleTimeout
	env := make(map[string]string, len(parent.Env))
	for k, v := range parent.Env {
		env[k] = v
	}
	publicPort, parentState := parent.PublicPort, parent.State
	networks := append([]string(nil), parent.Networks...)
	d.mu.Unlock()

	if vcpu <= 0 {
		vcpu = d.cfg.DefaultVCPU
	}
	if memMiB <= 0 {
		memMiB = d.cfg.DefaultMem
	}

	id := newSandboxID()
	unpin := func() {}
	if d.cfg.PinTemplate != nil {
		if rel := d.cfg.PinTemplate(template); rel != nil {
			unpin = rel
		}
	}
	t0 := d.clock.Now()
	forkCtx, forkSpan := observability.StartInternal(ctx, "runtime.Fork",
		attribute.String("sandbox_id", id), attribute.String("parent_id", parentID))
	// The child's memory and disk carry the parent's token; its own replaces it before anyone is told it exists.
	ident, identHash := d.identityFor(id)
	inst, info, err := forker.Fork(forkCtx, src, rt.BootSpec{
		SandboxID: id,
		Template:  rt.Template{Name: template, Image: image, Ports: ports},
		VCPU:      vcpu, MemMiB: memMiB, StorageMB: storageMB,
		Identity: ident,
	})
	observability.End(forkSpan, err)
	if err != nil {
		unpin()
		return ForkResult{}, fmt.Errorf("fork: %w", err)
	}
	// The child's own boot, not the parent's pause: the two are reported apart.
	bootMs := (d.clock.Now().Sub(t0) - info.ParentPaused).Milliseconds()

	now := d.clock.Now()
	ttl := spec.Timeout
	if ttl == 0 {
		ttl = d.cfg.HardTTL
	}
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	sb := &Sandbox{
		ID: id, Template: template, Name: spec.Name, OrgID: orgID, State: StateRunning,
		CreatedAt: now, ExpiresAt: expiresAt, LastActiveAt: now,
		PublicPort: publicPort, StartCmd: startCmd, Env: env,
		VCPU: vcpu, MemMiB: memMiB, StorageMB: storageMB,
		Image: image, Ports: ports,
		IdleTimeout:  idle,
		IdentityHash: identHash,
		// Same env, same egress, same networks: a copy of the sandbox, and a
		// child that silently left its parent's network would be the
		// surprising half of that.
		Networks: networks,
		// Not inherited: the forker owns the child, and an unasked-for child is
		// private. The HTTP layer has already refused a fork of a sandbox this
		// key cannot see, so nothing here needs the parent's own visibility.
		CreatedBy:  spec.CreatedBy,
		Visibility: forkVisibility(spec.Visibility, spec.CreatedBy),
		inst:       inst,
		meter:      newMeter(vcpu, memMiB, storageMB, now),
		unpin:      unpin,
	}
	d.mu.Lock()
	d.boxes[id] = sb
	d.mu.Unlock()
	if len(networks) > 0 {
		if err := d.pushNetworks(ctx, id); err != nil {
			d.log().Warn("child could not join the parent's networks", "sandbox_id", id, "error", err)
		}
	}
	d.mu.Lock()
	childInfo := sb.info()
	rec := d.recordOf(sb)
	d.mu.Unlock()
	span.SetAttributes(attribute.String("sandbox_id", id))
	d.persistRec(ctx, rec)
	d.recordEvent(ctx, orgID, id, "forked", template)
	d.log().Info("sandbox forked", "sandbox_id", id, "parent_id", parentID, "org_id", orgID,
		"parent_paused_ms", info.ParentPaused.Milliseconds(), "boot_ms", bootMs)
	return ForkResult{
		Child: childInfo, ParentID: parentID, ParentState: parentState,
		ParentPaused: info.ParentPaused, BootMs: bootMs,
	}, nil
}
