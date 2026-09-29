//go:build !core

package httpapi

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/mirror"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// agentFeature is the Server state only the agent surface uses, embedded into
// Server so the core build can replace it with an empty struct.
type agentFeature struct {
	// shares holds pending one-time share codes. In memory only: a live code is
	// credential-equivalent, and tokens are stored as hashes for the same reason.
	shares *shareCodes
}

func newAgentFeature() agentFeature { return agentFeature{shares: newShareCodes()} }

// StartAgentMirror begins copying agent journals into the control plane and
// returns a stop function. Started explicitly, like the cron sweeper.
func (s *Server) StartAgentMirror() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &mirror.Mirror{
		Store:   s.st,
		Targets: s.mirrorTargets,
		Sub: func(ctx context.Context, sandboxID string, sinceSeq uint64) (io.ReadCloser, error) {
			c, err := s.agentClient(sandboxID)
			if err != nil {
				return nil, err
			}
			return c.Events(ctx, sinceSeq)
		},
		// ErrUnsupported never clears, unlike ErrPaused.
		Permanent: func(err error) bool { return errors.Is(err, rt.ErrUnsupported) },
	}
	go m.Run(ctx)
	go s.pruneMirrorLoop(ctx)
	return cancel
}

// pruneMirrorLoop trims mirrored events, never turns, on a slow timer.
// The delete is idempotent and dated, so concurrent nodes need no leader.
func (s *Server) pruneMirrorLoop(ctx context.Context) {
	retention := time.Duration(envInt("AGENT_EVENT_RETENTION_DAYS",
		DefaultAgentEventRetentionDays)) * 24 * time.Hour
	if retention <= 0 {
		// 0 disables pruning; it must never mean "delete everything".
		return
	}
	t := time.NewTicker(mirrorPruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.st.PruneMirror(ctx, time.Now().Add(-retention)); err != nil {
				s.metrics.incAgentMirrorPruneError()
			} else if n > 0 {
				s.metrics.addAgentMirrorPruned(n)
			}
		}
	}
}

// mirrorPruneInterval is how often retention runs; short enough that the delete stays small.
const mirrorPruneInterval = 6 * time.Hour

// mirrorTargets lists running KindAgent sandboxes. Paused ones are excluded
// because reading one would wake it and bill the customer.
func (s *Server) mirrorTargets() []mirror.Target {
	var out []mirror.Target
	for _, sb := range s.d.List("") {
		if sb.State != daemon.StateRunning {
			continue
		}
		if t, ok := templates.Resolve(sb.Template); !ok || t.Kind != templates.KindAgent {
			continue
		}
		out = append(out, mirror.Target{SandboxID: sb.ID, OrgID: sb.OrgID})
	}
	return out
}
