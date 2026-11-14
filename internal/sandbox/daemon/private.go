package daemon

import (
	"context"
	"encoding/json"
	"errors"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// ErrPrivateUnsupported is a private target list this node cannot route.
var ErrPrivateUnsupported = errors.New("private access is not available on this sandbox's node")

// privateStore is the optional Persister capability holding a sandbox's private
// targets. The control plane writes the same row while a sandbox is paused, so
// the node reads it fresh on every push instead of keeping a copy that a later
// save could write back over it.
type privateStore interface {
	SandboxPrivate(ctx context.Context, id string) (string, error)
	SetSandboxPrivate(ctx context.Context, id, targetsJSON string) error
}

func (d *Daemon) privateOK() bool {
	_, stored := d.cfg.Store.(privateStore)
	return stored && d.cfg.Runtime != nil && d.cfg.Runtime.Capabilities().PrivateAccess
}

// SetPrivate replaces a sandbox's private targets. A running sandbox gets them
// at once; a paused one when it resumes.
func (d *Daemon) SetPrivate(ctx context.Context, id string, ts []sandbox.PrivateTarget) error {
	if err := sandbox.ValidPrivateTargets(ts); err != nil {
		return &sandbox.InvalidError{Reason: err.Error()}
	}
	if len(ts) > 0 && !d.privateOK() {
		return ErrPrivateUnsupported
	}
	d.mu.RLock()
	sb, ok := d.boxes[id]
	var inst rt.Instance
	if ok {
		inst = sb.inst
	}
	d.mu.RUnlock()
	if !ok {
		return rt.ErrNotFound
	}
	if err := d.routePrivate(ctx, inst, ts); err != nil {
		return err
	}
	ps, ok := d.cfg.Store.(privateStore)
	if !ok {
		return nil
	}
	if ts == nil {
		ts = []sandbox.PrivateTarget{}
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	return ps.SetSandboxPrivate(ctx, id, string(b))
}

// pushPrivate applies the stored targets to a sandbox's new instance: create,
// resume, adopt and checkpoint restore. A fresh instance routes nothing, so an
// empty list needs no push.
func (d *Daemon) pushPrivate(ctx context.Context, id string) error {
	ps, ok := d.cfg.Store.(privateStore)
	if !ok {
		return nil
	}
	d.mu.RLock()
	sb, ok := d.boxes[id]
	var inst rt.Instance
	if ok {
		inst = sb.inst
	}
	d.mu.RUnlock()
	if !ok {
		return rt.ErrNotFound
	}
	if inst == nil {
		return nil
	}
	raw, err := ps.SandboxPrivate(ctx, id)
	if err != nil || raw == "" || raw == "[]" {
		return err
	}
	var ts []sandbox.PrivateTarget
	if err := json.Unmarshal([]byte(raw), &ts); err != nil {
		return err
	}
	return d.routePrivate(ctx, inst, ts)
}

// routePrivate hands ts to a running instance; a paused one (nil) has nothing to route.
func (d *Daemon) routePrivate(ctx context.Context, inst rt.Instance, ts []sandbox.PrivateTarget) error {
	if inst == nil {
		return nil
	}
	p, can := inst.(rt.PrivateRouter)
	if !can || !d.privateOK() {
		if len(ts) == 0 {
			return nil
		}
		return ErrPrivateUnsupported
	}
	return p.SetPrivate(ctx, ts)
}
