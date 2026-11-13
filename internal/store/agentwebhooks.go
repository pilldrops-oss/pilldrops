package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Webhook storage. The signing secret is encrypted rather than hashed because
// every delivery needs it recoverable; it is shown to the customer once at creation.

// AgentWebhook is one configured hook.
type AgentWebhook struct {
	ID        string   `json:"id"`
	SandboxID string   `json:"sandbox_id"`
	OrgID     string   `json:"org_id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	// ConsecutiveFailures drives auto-disable; a success resets it.
	ConsecutiveFailures int       `json:"consecutive_failures"`
	DisabledReason      string    `json:"disabled_reason,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	CreatedBy           string    `json:"created_by,omitempty"`
	LastAt              time.Time `json:"last_at,omitempty"`
	LastStatus          int       `json:"last_status,omitempty"`
}

// DeliveryState is where one delivery stands.
type DeliveryState string

const (
	DeliveryPending DeliveryState = "pending"
	DeliverySent    DeliveryState = "sent"
	// DeliveryFailed is also the dead-letter queue.
	DeliveryFailed DeliveryState = "failed"
)

// AgentWebhookDelivery is one event's journey to one hook.
type AgentWebhookDelivery struct {
	ID         string        `json:"id"`
	WebhookID  string        `json:"webhook_id"`
	SandboxID  string        `json:"sandbox_id"`
	OrgID      string        `json:"org_id"`
	EventType  string        `json:"event_type"`
	EventSeq   uint64        `json:"event_seq"`
	Attempts   int           `json:"attempts"`
	State      DeliveryState `json:"state"`
	LastStatus int           `json:"last_status,omitempty"`
	LastError  string        `json:"last_error,omitempty"`
	NextAt     time.Time     `json:"next_at,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at,omitempty"`
}

// NewWebhookSecret mints a signing secret.
func NewWebhookSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "whsec_" + hex.EncodeToString(b[:])
}

// ErrWebhookLimit reports that the org or sandbox already holds its limit of hooks.
var ErrWebhookLimit = errors.New("webhook limit reached")

// CreateAgentWebhook stores a hook and returns it with its secret, once. An
// empty sandbox id makes it an org-level hook for sandbox lifecycle events.
// A limit above zero caps the hooks sharing its org and sandbox id; count and
// insert share one transaction serialized per org.
func (s *Store) CreateAgentWebhook(ctx context.Context, w AgentWebhook, secret string, limit int) (AgentWebhook, error) {
	if w.URL == "" {
		return AgentWebhook{}, fmt.Errorf("url is required")
	}
	if secret == "" {
		return AgentWebhook{}, fmt.Errorf("a signing secret is required")
	}
	if w.OrgID == "" {
		w.OrgID = "default"
	}
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	w.ID = "whk_" + hex.EncodeToString(idb[:])
	w.Enabled = true
	w.CreatedAt = time.Now().Truncate(time.Second)

	enc, err := s.crypt.seal(secret)
	if err != nil {
		return AgentWebhook{}, fmt.Errorf("encrypt webhook secret: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentWebhook{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if limit > 0 {
		// SQLite serializes writers already; Postgres needs the lock.
		if s.dialect == dialectPostgres {
			if _, err := tx.ExecContext(ctx, s.rebind(`SELECT pg_advisory_xact_lock(hashtext(?))`),
				"webhooks:"+w.OrgID); err != nil {
				return AgentWebhook{}, err
			}
		}
		var held int
		if err := tx.QueryRowContext(ctx, s.rebind(
			`SELECT COUNT(*) FROM agent_webhooks WHERE org_id = ? AND sandbox_id = ?`),
			w.OrgID, w.SandboxID).Scan(&held); err != nil {
			return AgentWebhook{}, err
		}
		if held >= limit {
			return AgentWebhook{}, ErrWebhookLimit
		}
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO agent_webhooks
		(id, sandbox_id, org_id, url, secret_enc, events, enabled, consecutive_failures,
		 disabled_reason, created_at, created_by, last_at, last_status)
		VALUES (?,?,?,?,?,?,?,0,'',?,?,0,0)`),
		w.ID, w.SandboxID, w.OrgID, w.URL, enc, strings.Join(w.Events, " "),
		true, w.CreatedAt.Unix(), w.CreatedBy); err != nil {
		return AgentWebhook{}, err
	}
	return w, tx.Commit()
}

// ListAgentWebhooks returns a sandbox's hooks, or the org-level ones for an
// empty sandbox id, never including the secret.
func (s *Store) ListAgentWebhooks(ctx context.Context, orgID, sandboxID string) ([]AgentWebhook, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, sandbox_id, org_id, url, events, enabled,
		consecutive_failures, disabled_reason, created_at, created_by, last_at, last_status
		FROM agent_webhooks WHERE org_id = ? AND sandbox_id = ? ORDER BY created_at DESC`),
		orgID, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentWebhook
	for rows.Next() {
		var (
			w             AgentWebhook
			events        string
			created, last int64
		)
		if err := rows.Scan(&w.ID, &w.SandboxID, &w.OrgID, &w.URL, &events, &w.Enabled,
			&w.ConsecutiveFailures, &w.DisabledReason, &created, &w.CreatedBy, &last,
			&w.LastStatus); err != nil {
			return nil, err
		}
		if events != "" {
			w.Events = strings.Fields(events)
		}
		w.CreatedAt, w.LastAt = fromUnix(created), fromUnix(last)
		out = append(out, w)
	}
	return out, rows.Err()
}

// AgentWebhookSecret decrypts one hook's signing secret. Kept off the listing
// path so a handler cannot leak it by returning the struct it already has.
func (s *Store) AgentWebhookSecret(ctx context.Context, id string) (string, error) {
	var enc string
	if err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT secret_enc FROM agent_webhooks WHERE id = ?`), id).Scan(&enc); err != nil {
		return "", err
	}
	return s.crypt.open(enc)
}

// DeleteAgentWebhook removes a hook, scoped to its org.
func (s *Store) DeleteAgentWebhook(ctx context.Context, orgID, sandboxID, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM agent_webhooks WHERE id = ? AND org_id = ? AND sandbox_id = ?`),
		id, orgID, sandboxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RecordWebhookResult updates a hook after a delivery attempt and auto-disables
// it after disableAfter consecutive failures. Returns true only on the call that
// flipped it, so the caller notifies exactly once.
func (s *Store) RecordWebhookResult(ctx context.Context, id string, status int, failed bool, disableAfter int) (disabled bool, err error) {
	now := time.Now().Unix()
	if !failed {
		_, err = s.db.ExecContext(ctx, s.rebind(`UPDATE agent_webhooks
			SET consecutive_failures = 0, last_at = ?, last_status = ? WHERE id = ?`),
			now, status, id)
		return false, err
	}

	var fails int
	if err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT consecutive_failures FROM agent_webhooks WHERE id = ?`), id).Scan(&fails); err != nil {
		return false, err
	}
	fails++
	if fails >= disableAfter {
		_, err = s.db.ExecContext(ctx, s.rebind(`UPDATE agent_webhooks
			SET consecutive_failures = ?, enabled = ?, disabled_reason = ?, last_at = ?, last_status = ?
			WHERE id = ? AND enabled = ?`),
			fails, false,
			fmt.Sprintf("disabled automatically after %d consecutive failed deliveries; "+
				"the endpoint did not accept anything. Fix it and re-enable.", fails),
			now, status, id, true)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	_, err = s.db.ExecContext(ctx, s.rebind(`UPDATE agent_webhooks
		SET consecutive_failures = ?, last_at = ?, last_status = ? WHERE id = ?`),
		fails, now, status, id)
	return false, err
}

// EnqueueDelivery records an event for a hook, idempotent on (webhook, sandbox,
// event_seq, event_type). Returns the delivery and whether it was newly created.
func (s *Store) EnqueueDelivery(ctx context.Context, d AgentWebhookDelivery) (AgentWebhookDelivery, bool, error) {
	if d.WebhookID == "" || d.SandboxID == "" || d.EventType == "" {
		return AgentWebhookDelivery{}, false, fmt.Errorf("webhook id, sandbox id and event type are required")
	}
	// Look first so a retry gets the existing id back rather than a unique violation.
	var existing AgentWebhookDelivery
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT id, attempts, state FROM agent_webhook_deliveries
		WHERE webhook_id = ? AND sandbox_id = ? AND event_seq = ? AND event_type = ?`),
		d.WebhookID, d.SandboxID, int64(d.EventSeq), d.EventType).
		Scan(&existing.ID, &existing.Attempts, &existing.State)
	if err == nil {
		d.ID, d.Attempts, d.State = existing.ID, existing.Attempts, existing.State
		return d, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AgentWebhookDelivery{}, false, err
	}

	var idb [10]byte
	_, _ = rand.Read(idb[:])
	d.ID = "whd_" + hex.EncodeToString(idb[:])
	if d.State == "" {
		d.State = DeliveryPending
	}
	now := time.Now().Truncate(time.Second)
	d.CreatedAt, d.UpdatedAt = now, now
	_, err = s.db.ExecContext(ctx, s.rebind(`INSERT INTO agent_webhook_deliveries
		(id, webhook_id, sandbox_id, org_id, event_type, event_seq, attempts, state,
		 last_status, last_error, next_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,0,?,0,'',?,?,?)`),
		d.ID, d.WebhookID, d.SandboxID, d.OrgID, d.EventType, int64(d.EventSeq),
		string(d.State), unixOrZero(d.NextAt), now.Unix(), now.Unix())
	if err != nil {
		return AgentWebhookDelivery{}, false, err
	}
	return d, true, nil
}

// RecordDeliveryAttempt advances one delivery.
func (s *Store) RecordDeliveryAttempt(ctx context.Context, id string, state DeliveryState, status int, errMsg string, nextAt time.Time) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`UPDATE agent_webhook_deliveries
		SET attempts = attempts + 1, state = ?, last_status = ?, last_error = ?,
		    next_at = ?, updated_at = ?
		WHERE id = ?`),
		string(state), status, truncErr(errMsg), unixOrZero(nextAt), time.Now().Unix(), id)
	return err
}

// ListFailedDeliveries is the dead-letter view, newest first.
func (s *Store) ListFailedDeliveries(ctx context.Context, orgID, sandboxID string, limit int) ([]AgentWebhookDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, webhook_id, sandbox_id, org_id, event_type,
		event_seq, attempts, state, last_status, last_error, next_at, created_at, updated_at
		FROM agent_webhook_deliveries
		WHERE org_id = ? AND sandbox_id = ? AND state = ?
		ORDER BY updated_at DESC LIMIT ?`), orgID, sandboxID, string(DeliveryFailed), limit)
	if err != nil {
		return nil, err
	}
	return scanDeliveries(rows)
}

// ClaimDueDeliveries leases pending deliveries that are due by pushing next_at
// forward. The conditional update gives each row to one caller, so several nodes
// can poll without a leader, and a crashed attempt comes back when the lease ends.
// Deliveries for skipHooks are left alone.
func (s *Store) ClaimDueDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int, skipHooks []string) ([]AgentWebhookDelivery, error) {
	q := `SELECT id, webhook_id, sandbox_id, org_id, event_type,
		event_seq, attempts, state, last_status, last_error, next_at, created_at, updated_at
		FROM agent_webhook_deliveries
		WHERE state = ? AND next_at <= ?`
	args := []any{string(DeliveryPending), now.Unix()}
	if len(skipHooks) > 0 {
		q += ` AND webhook_id NOT IN (?` + strings.Repeat(",?", len(skipHooks)-1) + `)`
		for _, id := range skipHooks {
			args = append(args, id)
		}
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(q+` ORDER BY next_at ASC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	due, err := scanDeliveries(rows)
	if err != nil {
		return nil, err
	}
	claimed := due[:0]
	for _, d := range due {
		res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE agent_webhook_deliveries SET next_at = ?
			WHERE id = ? AND state = ? AND next_at = ?`),
			now.Add(lease).Unix(), d.ID, string(DeliveryPending), unixOrZero(d.NextAt))
		if err != nil {
			return claimed, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			claimed = append(claimed, d)
		}
	}
	return claimed, nil
}

func scanDeliveries(rows *sql.Rows) ([]AgentWebhookDelivery, error) {
	defer rows.Close()

	var out []AgentWebhookDelivery
	for rows.Next() {
		var (
			d                           AgentWebhookDelivery
			state                       string
			seq, next, created, updated int64
		)
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.SandboxID, &d.OrgID, &d.EventType,
			&seq, &d.Attempts, &state, &d.LastStatus, &d.LastError, &next, &created,
			&updated); err != nil {
			return nil, err
		}
		d.State, d.EventSeq = DeliveryState(state), uint64(seq)
		d.NextAt, d.CreatedAt, d.UpdatedAt = fromUnix(next), fromUnix(created), fromUnix(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// truncErr bounds a stored error message; a receiver can return a megabyte of HTML.
func truncErr(s string) string {
	const max = 512
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// DeleteAgentWebhooksForSandbox removes every hook bound to a sandbox and its
// deliveries. Unlike turns, events and approvals, hooks are configuration, not
// history, and cannot fire once the machine is gone.
func (s *Store) DeleteAgentWebhooksForSandbox(ctx context.Context, sandboxID string) error {
	// By hook, not by sandbox: an org-level hook's deliveries name this sandbox too.
	if _, err := s.exec(ctx, `DELETE FROM agent_webhook_deliveries
		WHERE webhook_id IN (SELECT id FROM agent_webhooks WHERE sandbox_id = ?)`, sandboxID); err != nil {
		return err
	}
	_, err := s.exec(ctx, `DELETE FROM agent_webhooks WHERE sandbox_id = ?`, sandboxID)
	return err
}
