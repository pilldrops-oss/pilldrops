package daemon

import (
	"context"
	"errors"
	"fmt"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// ErrTagNetworksUnsupported is a create or patch naming network tags on a node
// whose runtime cannot partition them. Refused rather than ignored: a sandbox
// whose row says it is on a network its node cannot build is a lie the caller
// would act on.
var ErrTagNetworksUnsupported = errors.New("this node's runtime cannot put sandboxes on a shared network " +
	"(tag networks need firecracker with per-sandbox namespaces, FC_NETNS=1); " +
	"create without networks, or place the sandbox on a node that reports tag_networks")

// tagNetworksOK reports whether this node's runtime partitions tag networks.
func (d *Daemon) tagNetworksOK() bool {
	return d.cfg.Runtime != nil && d.cfg.Runtime.Capabilities().TagNetworks
}

// setKeys scopes each tag to its org. The runtime is handed opaque keys, so it
// never has to know what an org is and two orgs cannot join one network by
// picking the same word.
func setKeys(orgID string, tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, orgID+"/"+t)
	}
	return out
}

// pushNetworks hands a sandbox's current membership to its instance and records
// the address peers reach it on. Every path calls this one function (create,
// patch, rename, resume, fork, adopt), so a sandbox's kernel-level membership is
// a function of its row and not of the order things happened in. A paused
// sandbox has no instance and nothing to push: it is unreachable either way,
// and resume pushes for it.
func (d *Daemon) pushNetworks(ctx context.Context, id string) error {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	var (
		tags       []string
		org, slug  string
		inst       rt.Instance
		hadNetwork bool
	)
	if ok {
		tags, org, slug, inst = sb.Networks, sb.OrgID, sb.Slug, sb.inst
		hadNetwork = sb.PrivateIP != ""
	}
	d.mu.RUnlock()
	if !ok {
		return rt.ErrNotFound
	}
	if inst == nil {
		return nil
	}
	n, canNetwork := inst.(rt.Networker)
	if !canNetwork || !d.tagNetworksOK() {
		if len(tags) == 0 && !hadNetwork {
			return nil
		}
		return ErrTagNetworksUnsupported
	}
	ip, err := n.SetNetworks(ctx, rt.Networks{Sets: setKeys(org, tags), Slug: slug})
	if err != nil {
		return err
	}
	d.mu.Lock()
	if sb, ok := d.boxes[id]; ok {
		sb.PrivateIP = ip
	}
	d.mu.Unlock()
	return nil
}

// dropNetworks removes a sandbox from every network it joined. Called on
// destroy while the instance is still up; the runtime is expected to do the
// same for itself when it tears the sandbox down, and this is the belt to that
// braces — a membership that outlives its sandbox hands the next occupant of
// its address its predecessor's peers.
func (d *Daemon) dropNetworks(ctx context.Context, inst rt.Instance, id string) {
	n, ok := inst.(rt.Networker)
	if !ok {
		return
	}
	if _, err := n.SetNetworks(ctx, rt.Networks{}); err != nil {
		d.log().Warn("destroy: could not clear network membership", "sandbox_id", id, "error", err)
	}
}

// SetNetworks replaces a running sandbox's network tags. An empty list closes
// it off again. Takes effect on the running sandbox; no restart.
func (d *Daemon) SetNetworks(ctx context.Context, id string, tags []string) error {
	if err := sandbox.ValidNetworks(tags); err != nil {
		return &sandbox.InvalidError{Reason: err.Error()}
	}
	if len(tags) > 0 && !d.tagNetworksOK() {
		return ErrTagNetworksUnsupported
	}
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	prev := sb.Networks
	sb.Networks = tags
	rec := d.recordOf(sb)
	d.mu.Unlock()

	if err := d.pushNetworks(ctx, id); err != nil {
		// Roll back rather than leave the row claiming a membership the kernel
		// does not have, then re-push so the kernel matches the row again.
		d.mu.Lock()
		sb.Networks = prev
		d.mu.Unlock()
		_ = d.pushNetworks(ctx, id)
		return fmt.Errorf("set networks: %w", err)
	}
	d.persistRec(ctx, rec)
	return nil
}
