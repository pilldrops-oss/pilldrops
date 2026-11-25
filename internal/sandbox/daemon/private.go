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

// ErrPrivateRoutesUnsupported is a route list this node cannot carry.
var ErrPrivateRoutesUnsupported = errors.New("subnet and DNS grants are not available on this sandbox's node")

// privateRoutesStore holds a sandbox's subnet and DNS routes, read fresh on
// every push for the reason privateStore is.
type privateRoutesStore interface {
	SandboxPrivateRoutes(ctx context.Context, id string) (string, error)
	SetSandboxPrivateRoutes(ctx context.Context, id, routesJSON string) error
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

// pushPrivate applies the stored targets and routes to a sandbox's new
// instance: create, resume, adopt and checkpoint restore. A fresh instance
// routes nothing, so an empty list needs no push. Learned split-DNS addresses
// are not stored: the guest resolves again.
func (d *Daemon) pushPrivate(ctx context.Context, id string) error {
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
	if ps, ok := d.cfg.Store.(privateStore); ok {
		raw, err := ps.SandboxPrivate(ctx, id)
		if err != nil {
			return err
		}
		if raw != "" && raw != "[]" {
			var ts []sandbox.PrivateTarget
			if err := json.Unmarshal([]byte(raw), &ts); err != nil {
				return err
			}
			if err := d.routePrivate(ctx, inst, ts); err != nil {
				return err
			}
		}
	}
	rs, ok := d.cfg.Store.(privateRoutesStore)
	if !ok {
		return nil
	}
	raw, err := rs.SandboxPrivateRoutes(ctx, id)
	if err != nil || raw == "" {
		return err
	}
	var r sandbox.PrivateRoutes
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return err
	}
	if r.Empty() {
		return nil
	}
	return d.routePrivateRoutes(ctx, inst, r)
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

func (d *Daemon) privateRoutesOK() bool {
	_, stored := d.cfg.Store.(privateRoutesStore)
	return stored && d.cfg.Runtime != nil && d.cfg.Runtime.Capabilities().PrivateRoutes
}

// SetPrivateRoutes replaces a sandbox's subnet and split-DNS routes. A running
// sandbox gets them at once; a paused one when it resumes.
func (d *Daemon) SetPrivateRoutes(ctx context.Context, id string, r sandbox.PrivateRoutes) error {
	if err := sandbox.ValidPrivateRoutes(r); err != nil {
		return &sandbox.InvalidError{Reason: err.Error()}
	}
	if !r.Empty() && !d.privateRoutesOK() {
		return ErrPrivateRoutesUnsupported
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
	if err := d.routePrivateRoutes(ctx, inst, r); err != nil {
		return err
	}
	ps, ok := d.cfg.Store.(privateRoutesStore)
	if !ok {
		return nil
	}
	if r.Subnets == nil {
		r.Subnets = []string{}
	}
	if r.DNS == nil {
		r.DNS = []string{}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return ps.SetSandboxPrivateRoutes(ctx, id, string(b))
}

// routePrivateRoutes hands r to a running instance; a paused one has nothing to route.
func (d *Daemon) routePrivateRoutes(ctx context.Context, inst rt.Instance, r sandbox.PrivateRoutes) error {
	if inst == nil {
		return nil
	}
	p, can := inst.(rt.PrivateRouteSetter)
	if !can || !d.privateRoutesOK() {
		if r.Empty() {
			return nil
		}
		return ErrPrivateRoutesUnsupported
	}
	return p.SetPrivateRoutes(ctx, r)
}
