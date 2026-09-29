// Package agentd is the in-sandbox agent supervisor. It owns a long-lived agent
// process and an append-only event journal that streaming, webhooks, transcript
// and audit all read from. The node reaches it over TCP behind a per-sandbox token.
package agentd

import (
	"encoding/json"
	"time"
)

// Event is one journal record. There is no sandbox id; the node attaches one
// when mirroring to the control plane.
type Event struct {
	// Seq is monotonic and gap-free within a sandbox; clients resume from it.
	Seq uint64    `json:"seq"`
	At  time.Time `json:"at"`
	// Type is one of the constants below. Readers must tolerate unknown types.
	Type   string `json:"type"`
	TurnID string `json:"turn_id,omitempty"`
	// Step orders events within a turn for display. Zero means "not a step".
	Step int `json:"step,omitempty"`
	// Payload is type-specific raw JSON so replay can stream bytes without re-encoding.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Event types, shared by streaming, webhooks, transcript, timeline and audit.
const (
	// Session lifecycle.
	EventSessionStarting = "session.starting"
	EventSessionReady    = "session.ready"
	// EventSessionDegraded means the agent runs with less capability than
	// intended. Always carries a reason.
	EventSessionDegraded = "session.degraded"
	EventSessionStopped  = "session.stopped"
	EventSessionConfig   = "session.config_changed"

	// Turn lifecycle.
	EventTurnQueued   = "turn.queued"
	EventTurnStarted  = "turn.started"
	EventTurnThinking = "turn.thinking"
	// EventTurnOutput is the agent's visible output: high frequency, coalesced on
	// the wire, and never delivered to webhooks.
	EventTurnOutput = "turn.output"
	// EventTurnOutputTruncated says capture hit its per-turn ceiling.
	EventTurnOutputTruncated = "turn.output_truncated"
	EventTurnFinished        = "turn.finished"
	EventTurnFailed          = "turn.failed"
	EventTurnInterrupted     = "turn.interrupted"

	// Tool calls, available only from agents with a structured event stream.
	EventToolStarted  = "tool.started"
	EventToolFinished = "tool.finished"
	EventToolFailed   = "tool.failed"

	// EventSpendThreshold announces crossing a percentage of the spend cap.
	EventSpendThreshold = "spend.threshold"

	// Human-in-the-loop.
	EventApprovalRequested = "approval.requested"
	EventApprovalGranted   = "approval.granted"
	EventApprovalDenied    = "approval.denied"
	EventApprovalTimeout   = "approval.timeout"
)

// TurnState is where a turn is in its life. Persisted in the queue, so the
// strings are a storage format: renaming one is a migration, not a refactor.
type TurnState string

const (
	TurnQueued      TurnState = "queued"
	TurnRunning     TurnState = "running"
	TurnAwaiting    TurnState = "awaiting_approval"
	TurnDone        TurnState = "done"
	TurnFailed      TurnState = "failed"
	TurnInterrupted TurnState = "interrupted"
)

// Terminal reports whether a turn has finished for good.
func (s TurnState) Terminal() bool {
	switch s {
	case TurnDone, TurnFailed, TurnInterrupted:
		return true
	}
	return false
}

// Mode is how much of an agent's work the supervisor can see.
type Mode string

const (
	// ModeRich parses a structured event stream: per-tool steps and token cost.
	ModeRich Mode = "rich"
	// ModeBasic captures terminal output only: text, turn boundaries and an exit code.
	ModeBasic Mode = "basic"
)
