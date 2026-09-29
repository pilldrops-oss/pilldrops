package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Control-plane mirror of the sandbox journal. Every write must be safe to
// re-run: the mirror resumes from a cursor and restarts mid-batch are normal.

// MirroredEvent is one journal event as the control plane keeps it.
type MirroredEvent struct {
	SandboxID string          `json:"sandbox_id"`
	Seq       uint64          `json:"seq"`
	OrgID     string          `json:"org_id"`
	TurnID    string          `json:"turn_id,omitempty"`
	Type      string          `json:"type"`
	Step      int             `json:"step,omitempty"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// MirroredTurn is one turn as the control plane keeps it.
type MirroredTurn struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandbox_id"`
	OrgID     string `json:"org_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Request   string `json:"request"`
	Response  string `json:"response,omitempty"`
	State     string `json:"state"`
	Mode      string `json:"mode,omitempty"`
	Source    string `json:"source,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Error     string `json:"error,omitempty"`
	// CostUSD is nil when the agent reported nothing; nil means unknown, not 0.
	CostUSD   *float64  `json:"cost_usd,omitempty"`
	QueuedAt  time.Time `json:"queued_at"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	SeqStart  uint64    `json:"seq_start,omitempty"`
	SeqEnd    uint64    `json:"seq_end,omitempty"`
}

// MirrorCursor returns the highest event seq mirrored for a sandbox, or 0 when none.
// Derived from MAX(seq) rather than a stored cursor so it can never disagree with the rows.
func (s *Store) MirrorCursor(ctx context.Context, sandboxID string) (uint64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT MAX(seq) FROM agent_events WHERE sandbox_id = ?`), sandboxID).Scan(&seq)
	if err != nil {
		return 0, err
	}
	if !seq.Valid || seq.Int64 < 0 {
		return 0, nil
	}
	return uint64(seq.Int64), nil
}

// MirrorEvents writes a batch idempotently and returns how many rows were new.
func (s *Store) MirrorEvents(ctx context.Context, events []MirroredEvent) (inserted int, err error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, s.rebind(`INSERT INTO agent_events
		(sandbox_id, seq, org_id, turn_id, type, step, at, payload)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (sandbox_id, seq) DO NOTHING`))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for _, e := range events {
		if e.SandboxID == "" || e.Seq == 0 || e.Type == "" {
			// Skip malformed rows rather than failing the whole batch.
			continue
		}
		res, err := stmt.ExecContext(ctx, e.SandboxID, int64(e.Seq), e.OrgID, e.TurnID, e.Type,
			e.Step, unixOrZero(e.At), string(e.Payload))
		if err != nil {
			return inserted, fmt.Errorf("mirror event %s/%d: %w", e.SandboxID, e.Seq, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			inserted += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// MirrorTurn upserts one turn. Turns mutate while they run, so later writes
// overwrite, except that a nil CostUSD never erases a recorded cost.
func (s *Store) MirrorTurn(ctx context.Context, t MirroredTurn) error {
	if t.ID == "" || t.SandboxID == "" {
		return fmt.Errorf("turn id and sandbox id are required")
	}
	var existingCost sql.NullFloat64
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT cost_usd FROM agent_turns WHERE id = ?`), t.ID).Scan(&existingCost)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.ExecContext(ctx, s.rebind(`INSERT INTO agent_turns
			(id, sandbox_id, org_id, thread_id, session_id, request, response, state,
			 mode, source, exit_code, error, cost_usd, queued_at, started_at, ended_at,
			 seq_start, seq_end)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			t.ID, t.SandboxID, t.OrgID, t.ThreadID, t.SessionID, t.Request, t.Response,
			t.State, t.Mode, t.Source, t.ExitCode, t.Error, t.CostUSD,
			unixOrZero(t.QueuedAt), unixOrZero(t.StartedAt), unixOrZero(t.EndedAt),
			int64(t.SeqStart), int64(t.SeqEnd))
		return err
	case err != nil:
		return err
	}

	cost := t.CostUSD
	if cost == nil && existingCost.Valid {
		v := existingCost.Float64
		cost = &v
	}
	_, err = s.db.ExecContext(ctx, s.rebind(`UPDATE agent_turns SET
		org_id = ?, thread_id = ?, session_id = ?, request = ?, response = ?, state = ?,
		mode = ?, source = ?, exit_code = ?, error = ?, cost_usd = ?,
		queued_at = ?, started_at = ?, ended_at = ?, seq_start = ?, seq_end = ?
		WHERE id = ?`),
		t.OrgID, t.ThreadID, t.SessionID, t.Request, t.Response, t.State, t.Mode,
		t.Source, t.ExitCode, t.Error, cost,
		unixOrZero(t.QueuedAt), unixOrZero(t.StartedAt), unixOrZero(t.EndedAt),
		int64(t.SeqStart), int64(t.SeqEnd), t.ID)
	return err
}

// ListMirroredTurns returns a sandbox's turns, newest first, scoped to orgID.
func (s *Store) ListMirroredTurns(ctx context.Context, orgID, sandboxID string, limit int) ([]MirroredTurn, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, sandbox_id, org_id, thread_id,
		session_id, request, response, state, mode, source, exit_code, error, cost_usd,
		queued_at, started_at, ended_at, seq_start, seq_end
		FROM agent_turns WHERE org_id = ? AND sandbox_id = ?
		ORDER BY queued_at DESC, id DESC LIMIT ?`), orgID, sandboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MirroredTurn
	for rows.Next() {
		var (
			t                      MirroredTurn
			exit                   sql.NullInt64
			cost                   sql.NullFloat64
			queued, started, ended int64
			seqStart, seqEnd       int64
		)
		if err := rows.Scan(&t.ID, &t.SandboxID, &t.OrgID, &t.ThreadID, &t.SessionID,
			&t.Request, &t.Response, &t.State, &t.Mode, &t.Source, &exit, &t.Error,
			&cost, &queued, &started, &ended, &seqStart, &seqEnd); err != nil {
			return nil, err
		}
		if exit.Valid {
			v := int(exit.Int64)
			t.ExitCode = &v
		}
		if cost.Valid {
			v := cost.Float64
			t.CostUSD = &v
		}
		t.QueuedAt, t.StartedAt, t.EndedAt = fromUnix(queued), fromUnix(started), fromUnix(ended)
		t.SeqStart, t.SeqEnd = uint64(seqStart), uint64(seqEnd)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListMirroredEvents returns a sandbox's events after sinceSeq, ascending, so a
// client can fall back from the live stream with the same cursor.
func (s *Store) ListMirroredEvents(ctx context.Context, orgID, sandboxID string, sinceSeq uint64, limit int) ([]MirroredEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT sandbox_id, seq, org_id, turn_id, type,
		step, at, payload FROM agent_events
		WHERE org_id = ? AND sandbox_id = ? AND seq > ?
		ORDER BY seq ASC LIMIT ?`), orgID, sandboxID, int64(sinceSeq), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MirroredEvent
	for rows.Next() {
		var (
			e       MirroredEvent
			seq, at int64
			payload string
		)
		if err := rows.Scan(&e.SandboxID, &seq, &e.OrgID, &e.TurnID, &e.Type,
			&e.Step, &at, &payload); err != nil {
			return nil, err
		}
		e.Seq, e.At = uint64(seq), fromUnix(at)
		if payload != "" {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneMirror deletes events older than a cutoff and reports how many went.
// Turns are deliberately kept: they are the record that something happened and what it cost.
func (s *Store) PruneMirror(ctx context.Context, before time.Time) (int64, error) {
	for _, t := range RetentionTargets {
		if t.Name == "agent_events" {
			return s.PruneOlderThan(ctx, t, before.Unix())
		}
	}
	return 0, fmt.Errorf("no agent_events retention target is declared")
}

func fromUnix(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}
