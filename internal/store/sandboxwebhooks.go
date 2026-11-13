package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Org-level webhooks report sandbox lifecycle events. They live in the agent
// webhook tables with an empty sandbox_id, so one delivery loop serves both.

// SandboxWebhookEvents are the events an org-level webhook can subscribe to.
var SandboxWebhookEvents = []string{
	"sandbox.created", "sandbox.ended", "sandbox.paused", "sandbox.resumed",
	"sandbox.checkpointed", "sandbox.forked", "sandbox.restarted",
	"sandbox.crashed", "sandbox.out_of_memory", "sandbox.unreachable",
}

// sandboxWebhookEvent maps a recorded lifecycle event to its public name, or ""
// for one that is not delivered.
func (s *Store) sandboxWebhookEvent(ctx context.Context, r EventRecord) (string, error) {
	switch r.Event {
	case "created":
		return "sandbox.created", nil
	case "destroyed", "exited":
		return "sandbox.ended", nil
	case "oom":
		// The same name is recorded for a kill inside a sandbox that lives on and
		// for one that ended; only the ended one has a history row by now.
		_, ended, err := s.GetHistory(ctx, r.SandboxID)
		if err != nil {
			return "", err
		}
		if ended {
			return "sandbox.ended", nil
		}
		return "sandbox.out_of_memory", nil
	case "paused", "standby":
		return "sandbox.paused", nil
	case "resumed":
		return "sandbox.resumed", nil
	case "checkpointed":
		return "sandbox.checkpointed", nil
	case "forked":
		return "sandbox.forked", nil
	case "restarted":
		return "sandbox.restarted", nil
	case "crash":
		return "sandbox.crashed", nil
	case "unreachable":
		return "sandbox.unreachable", nil
	}
	return "", nil
}

// enqueueSandboxWebhooks queues one delivery per enabled org hook subscribed to
// a lifecycle event.
func (s *Store) enqueueSandboxWebhooks(ctx context.Context, r EventRecord) error {
	hooks, err := s.ListAgentWebhooks(ctx, r.OrgID, "")
	if err != nil || len(hooks) == 0 {
		return err
	}
	name, err := s.sandboxWebhookEvent(ctx, r)
	if err != nil || name == "" {
		return err
	}
	// One statement for every hook: this runs inside sandbox create and other
	// lifecycle calls, so a round trip per hook made them slower.
	now := time.Now().Unix()
	var values []string
	var args []any
	for _, h := range hooks {
		if !h.Enabled || !slices.Contains(h.Events, name) {
			continue
		}
		var idb [10]byte
		_, _ = rand.Read(idb[:])
		// The event time stands in for the sequence number agent events have.
		values = append(values, "(?,?,?,?,?,?,0,?,0,'',?,?,?)")
		args = append(args, "whd_"+hex.EncodeToString(idb[:]), h.ID, r.SandboxID, r.OrgID,
			name, r.At, string(DeliveryPending), now, now, now)
	}
	if len(values) == 0 {
		return nil
	}
	_, err = s.exec(ctx, `INSERT INTO agent_webhook_deliveries
		(id, webhook_id, sandbox_id, org_id, event_type, event_seq, attempts, state,
		 last_status, last_error, next_at, created_at, updated_at)
		VALUES `+strings.Join(values, ",")+`
		ON CONFLICT (webhook_id, sandbox_id, event_seq, event_type) DO NOTHING`, args...)
	if err != nil {
		return fmt.Errorf("queue %s: %w", name, err)
	}
	return nil
}

// SandboxEventTemplate returns the template recorded with a sandbox's event at
// a given time, or "" when the event is no longer stored.
func (s *Store) SandboxEventTemplate(ctx context.Context, orgID, sandboxID string, at int64) (string, error) {
	var template string
	err := s.queryRow(ctx, `SELECT template FROM sandbox_events
		WHERE org_id = ? AND sandbox_id = ? AND at = ? LIMIT 1`, orgID, sandboxID, at).Scan(&template)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return template, err
}

// SetAgentWebhookEnabled pauses or resumes a hook. Resuming clears the failure
// count, so a hook that was turned off automatically gets a fresh run.
func (s *Store) SetAgentWebhookEnabled(ctx context.Context, orgID, sandboxID, id string, enabled bool) error {
	q := `UPDATE agent_webhooks SET enabled = ? WHERE id = ? AND org_id = ? AND sandbox_id = ?`
	if enabled {
		q = `UPDATE agent_webhooks SET enabled = ?, consecutive_failures = 0, disabled_reason = ''
			WHERE id = ? AND org_id = ? AND sandbox_id = ?`
	}
	res, err := s.exec(ctx, q, enabled, id, orgID, sandboxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListOrgWebhookDeliveries returns the recent deliveries of an org's
// sandbox-event hooks, in every state, newest first.
func (s *Store) ListOrgWebhookDeliveries(ctx context.Context, orgID string, limit int) ([]AgentWebhookDelivery, error) {
	rows, err := s.query(ctx, `SELECT id, webhook_id, sandbox_id, org_id, event_type,
		event_seq, attempts, state, last_status, last_error, next_at, created_at, updated_at
		FROM agent_webhook_deliveries
		WHERE org_id = ? AND webhook_id IN (SELECT id FROM agent_webhooks WHERE org_id = ? AND sandbox_id = '')
		ORDER BY created_at DESC, id DESC LIMIT ?`, orgID, orgID, limit)
	if err != nil {
		return nil, err
	}
	return scanDeliveries(rows)
}
