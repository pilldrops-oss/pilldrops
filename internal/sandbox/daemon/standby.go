package daemon

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/pilldrops-oss/pilldrops/internal/observability"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Standby freezes a running sandbox in RAM: its memory, processes and open
// sockets all stay, and a wake is milliseconds rather than a restore. The node
// still holds the RAM, so a standby sandbox keeps its admission slot and is
// still billed for memory — the compute is what stops. Pause is the tier below:
// the memory is written to disk and freed.
//
// Serialized per sandbox on opMu and idempotent.
func (d *Daemon) Standby(ctx context.Context, id string) (took time.Duration, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Standby", attribute.String("sandbox_id", id))
	defer func() { observability.End(span, err) }()

	sb, release, err := d.acquireOp(id)
	if err != nil {
		return 0, err
	}
	defer release()

	d.mu.Lock()
	if sb.State == StateStandby {
		d.mu.Unlock()
		return 0, nil
	}
	if !canTransition(sb.State, "standby") {
		st := sb.State
		d.mu.Unlock()
		return 0, fmt.Errorf("cannot standby from state %s", st)
	}
	inst := sb.inst
	d.mu.Unlock()

	fr, ok := inst.(rt.Freezer)
	if !ok {
		return 0, fmt.Errorf("%w: the %s runtime cannot freeze a sandbox in RAM",
			rt.ErrUnsupported, d.RuntimeName())
	}
	t0 := d.clock.Now()
	if err := fr.Freeze(ctx); err != nil {
		return 0, fmt.Errorf("standby: %w", err)
	}
	took = d.clock.Now().Sub(t0)

	d.mu.Lock()
	u := sb.meter.sample(d.clock.Now())
	sb.meter.standby(d.clock.Now())
	sb.State = StateStandby
	orgID, template := sb.OrgID, sb.Template
	rec := d.recordOf(sb)
	d.mu.Unlock()

	d.flushUsage(ctx, id, orgID, u)
	d.persistRec(ctx, rec)
	d.recordEvent(ctx, orgID, id, "standby", template)
	d.log().Info("sandbox on standby", "sandbox_id", id, "org_id", orgID, "standby_ms", took.Milliseconds())
	return took, nil
}

// thaw is Resume's standby half: the instance is still there, so the wake is a
// Thaw rather than a restore. The caller has already taken opMu and set
// StateResuming, and hands the instance in.
func (d *Daemon) thaw(ctx context.Context, id string, sb *Sandbox, inst rt.Instance) (time.Duration, error) {
	fr, ok := inst.(rt.Freezer)
	if !ok {
		d.mu.Lock()
		sb.State = StateStandby
		d.mu.Unlock()
		return 0, fmt.Errorf("%w: the %s runtime cannot thaw a sandbox", rt.ErrUnsupported, d.RuntimeName())
	}
	t0 := d.clock.Now()
	if err := fr.Thaw(ctx); err != nil {
		d.mu.Lock()
		sb.State = StateStandby // revert so a retry can wake it again
		d.mu.Unlock()
		return 0, fmt.Errorf("thaw: %w", err)
	}
	took := d.clock.Now().Sub(t0)
	d.metrics.ResumeDuration.Observe(took)

	d.mu.Lock()
	sb.State = StateRunning
	sb.LastActiveAt = d.clock.Now()
	sb.meter.resume(d.clock.Now())
	orgID, template := sb.OrgID, sb.Template
	rec := d.recordOf(sb)
	d.mu.Unlock()

	// No StartServer: the app never stopped, it was only frozen.
	d.persistRec(ctx, rec)
	d.recordEvent(ctx, orgID, id, "resumed", template)
	d.log().Info("sandbox thawed", "sandbox_id", id, "org_id", orgID, "wake_ms", took.Milliseconds())
	return took, nil
}
