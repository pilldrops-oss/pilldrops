package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Durable audit copy of the approval trail. The gate inside the sandbox is
// authoritative; nothing here can release a blocked call.

// ApprovalState is the outcome of one approval.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalAllowed  ApprovalState = "allowed"
	ApprovalDenied   ApprovalState = "denied"
	ApprovalTimedOut ApprovalState = "timed_out"
)

// AgentApproval is one row of the trail.
type AgentApproval struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandbox_id"`
	OrgID     string `json:"org_id"`
	TurnID    string `json:"turn_id,omitempty"`
	Tool      string `json:"tool"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Signature string `json:"signature,omitempty"`

	// Raw arguments are never stored; file contents routinely carry credentials.
	ArgsDigest  string `json:"args_digest,omitempty"`
	ArgsPreview string `json:"args_preview,omitempty"`

	RequestedAt time.Time     `json:"requested_at"`
	State       ApprovalState `json:"state"`
	DecidedBy   string        `json:"decided_by,omitempty"`
	DecidedAt   time.Time     `json:"decided_at,omitempty"`
	Remember    string        `json:"remember,omitempty"`
	Reason      string        `json:"reason,omitempty"`
}

// Decided reports whether this approval has an outcome.
func (a AgentApproval) Decided() bool { return a.State != ApprovalPending }

// ValidApprovalState reports whether s is a state this system records.
func ValidApprovalState(s ApprovalState) bool {
	switch s {
	case ApprovalPending, ApprovalAllowed, ApprovalDenied, ApprovalTimedOut:
		return true
	}
	return false
}

// UpsertAgentApproval records or advances one approval. Forward-only: the same
// approval arrives several times in no guaranteed order, so a decided row keeps
// its first outcome and only fills in empty bookkeeping fields.
func (s *Store) UpsertAgentApproval(ctx context.Context, a AgentApproval) error {
	if a.ID == "" || a.SandboxID == "" {
		return fmt.Errorf("approval id and sandbox id are required")
	}
	if a.State == "" {
		a.State = ApprovalPending
	}
	if !ValidApprovalState(a.State) {
		return fmt.Errorf("unknown approval state %q", a.State)
	}

	var (
		existingState  string
		existingFields = AgentApproval{}
	)
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT state, turn_id, tool, tool_use_id, signature,
		args_digest, args_preview, decided_by, decided_at, remember, reason
		FROM agent_approvals WHERE id = ?`), a.ID).Scan(
		&existingState, &existingFields.TurnID, &existingFields.Tool, &existingFields.ToolUseID,
		&existingFields.Signature, &existingFields.ArgsDigest, &existingFields.ArgsPreview,
		&existingFields.DecidedBy, new(int64), &existingFields.Remember, &existingFields.Reason)

	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.ExecContext(ctx, s.rebind(`INSERT INTO agent_approvals
			(id, sandbox_id, org_id, turn_id, tool, tool_use_id, signature,
			 args_digest, args_preview, requested_at, state, decided_by, decided_at,
			 remember, reason)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			a.ID, a.SandboxID, a.OrgID, a.TurnID, a.Tool, a.ToolUseID, a.Signature,
			a.ArgsDigest, a.ArgsPreview, unixOrZero(a.RequestedAt), string(a.State),
			a.DecidedBy, unixOrZero(a.DecidedAt), a.Remember, a.Reason)
		return err
	}
	if err != nil {
		return err
	}

	if ApprovalState(existingState) != ApprovalPending {
		_, err = s.db.ExecContext(ctx, s.rebind(`UPDATE agent_approvals SET
			turn_id = ?, tool = ?, tool_use_id = ?, signature = ?,
			args_digest = ?, args_preview = ?
			WHERE id = ?`),
			firstNonEmpty(existingFields.TurnID, a.TurnID),
			firstNonEmpty(existingFields.Tool, a.Tool),
			firstNonEmpty(existingFields.ToolUseID, a.ToolUseID),
			firstNonEmpty(existingFields.Signature, a.Signature),
			firstNonEmpty(existingFields.ArgsDigest, a.ArgsDigest),
			firstNonEmpty(existingFields.ArgsPreview, a.ArgsPreview),
			a.ID)
		return err
	}

	_, err = s.db.ExecContext(ctx, s.rebind(`UPDATE agent_approvals SET
		turn_id = ?, tool = ?, tool_use_id = ?, signature = ?,
		args_digest = ?, args_preview = ?, state = ?, decided_by = ?, decided_at = ?,
		remember = ?, reason = ?
		WHERE id = ?`),
		firstNonEmpty(a.TurnID, existingFields.TurnID),
		firstNonEmpty(a.Tool, existingFields.Tool),
		firstNonEmpty(a.ToolUseID, existingFields.ToolUseID),
		firstNonEmpty(a.Signature, existingFields.Signature),
		firstNonEmpty(a.ArgsDigest, existingFields.ArgsDigest),
		firstNonEmpty(a.ArgsPreview, existingFields.ArgsPreview),
		string(a.State), a.DecidedBy, unixOrZero(a.DecidedAt), a.Remember, a.Reason,
		a.ID)
	return err
}

// ListAgentApprovals returns a sandbox's trail, newest first. orgID is a tenancy
// boundary: sandbox ids alone are guessable.
func (s *Store) ListAgentApprovals(ctx context.Context, orgID, sandboxID string, limit int) ([]AgentApproval, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, sandbox_id, org_id, turn_id, tool,
		tool_use_id, signature, args_digest, args_preview, requested_at, state,
		decided_by, decided_at, remember, reason
		FROM agent_approvals WHERE org_id = ? AND sandbox_id = ?
		ORDER BY requested_at DESC, id DESC LIMIT ?`), orgID, sandboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanApprovals(rows)
}

// CountAgentApprovals returns how many of each state a sandbox has recorded.
func (s *Store) CountAgentApprovals(ctx context.Context, orgID, sandboxID string) (map[ApprovalState]int, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT state, COUNT(*) FROM agent_approvals
		WHERE org_id = ? AND sandbox_id = ? GROUP BY state`), orgID, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ApprovalState]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[ApprovalState(st)] = n
	}
	return out, rows.Err()
}

func scanApprovals(rows *sql.Rows) ([]AgentApproval, error) {
	var out []AgentApproval
	for rows.Next() {
		var (
			a               AgentApproval
			state           string
			requested, decd int64
		)
		if err := rows.Scan(&a.ID, &a.SandboxID, &a.OrgID, &a.TurnID, &a.Tool,
			&a.ToolUseID, &a.Signature, &a.ArgsDigest, &a.ArgsPreview, &requested,
			&state, &a.DecidedBy, &decd, &a.Remember, &a.Reason); err != nil {
			return nil, err
		}
		a.State = ApprovalState(state)
		if requested > 0 {
			a.RequestedAt = time.Unix(requested, 0).UTC()
		}
		if decd > 0 {
			a.DecidedAt = time.Unix(decd, 0).UTC()
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// unixOrZero stores a zero time as 0 rather than the year 1 epoch.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// firstNonEmpty is defined in credits.go.
