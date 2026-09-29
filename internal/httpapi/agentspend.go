//go:build !core

package httpapi

import (
	"context"
	"encoding/json"
	"time"
)

// spendCheckInterval is how often the spend guard polls. A sandbox at its cap
// stops accepting work but keeps billing machine time, so it gets paused.
const spendCheckInterval = 60 * time.Second

// spendPauseReason is the audit entry text; named so a test can assert it.
const spendPauseReason = "the spend cap was reached, so the workstation was paused to stop " +
	"machine charges. Raise or clear the cap, then resume."

// StartSpendGuard pauses agent sandboxes that have reached their spend cap and
// returns a stop function. Started explicitly, like the cron sweeper.
func (s *Server) StartSpendGuard() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(spendCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.pauseOverspentAgents(ctx)
			}
		}
	}()
	return cancel
}

// pauseOverspentAgents is one pass.
func (s *Server) pauseOverspentAgents(ctx context.Context) {
	for _, tgt := range s.mirrorTargets() {
		if ctx.Err() != nil {
			return
		}
		s.pauseIfOverspent(ctx, tgt.SandboxID, tgt.OrgID)
	}
}

// pauseIfOverspent pauses one sandbox when its cap is reached and it is safe to stop.
func (s *Server) pauseIfOverspent(ctx context.Context, id, org string) {
	c, err := s.agentClient(id)
	if err != nil {
		return // paused already, gone, or unreachable
	}
	st, err := c.Status(ctx)
	if err != nil {
		// Unreadable is not the same as exceeded.
		return
	}
	if st.Spend == nil || !st.Spend.Exceeded {
		return
	}

	// Never pause mid-turn; the queue is frozen, so waiting costs at most one turn.
	if safe, reason := c.SnapshotSafe(ctx); !safe {
		s.metrics.incAgentSpendPauseDeferred()
		_ = reason // surfaced via status
		return
	}

	if err := s.d.Pause(ctx, id); err != nil {
		s.metrics.incAgentSpendPauseError()
		return
	}
	s.metrics.incAgentSpendPause()
	// Attributed to the system: no request made this.
	detail, _ := json.Marshal(map[string]any{
		"spent_usd": st.Spend.SpentUSD, "cap_usd": st.Spend.CapUSD,
		"reason": spendPauseReason,
	})
	_ = s.st.RecordAuditDetail(ctx, org, "system:spend-guard", "agent.spend.autopause", id, string(detail))
}
