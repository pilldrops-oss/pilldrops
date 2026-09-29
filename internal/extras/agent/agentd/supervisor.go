package agentd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SessionState is what the workstation's agent is doing, as clients see it.
type SessionState string

const (
	StateStarting SessionState = "starting"
	StateIdle     SessionState = "idle"
	StateBusy     SessionState = "busy"
	// StateDegraded means turns still run with reduced capability; it always carries a reason.
	StateDegraded SessionState = "degraded"
	StateStopped  SessionState = "stopped"
)

// defaultPollInterval is the backstop for a missed wake notification.
const defaultPollInterval = 2 * time.Second

// Status is the snapshot the node serves as GET /agent/status.
type Status struct {
	State          SessionState `json:"state"`
	Mode           Mode         `json:"mode"`
	DegradedReason string       `json:"degraded_reason,omitempty"`
	CurrentTurn    *Turn        `json:"current_turn,omitempty"`
	QueueDepth     int          `json:"queue_depth"`
	LastSeq        uint64       `json:"last_seq"`
	// Restarts counts agent processes replaced after an unexpected exit.
	Restarts int `json:"restarts"`
	// Spend is the cap and spend against it; absent when no budget is configured.
	Spend *BudgetSnapshot `json:"spend,omitempty"`
	// AgentVersion is the CLI version the agent runs.
	AgentVersion string `json:"agent_version,omitempty"`
	// Autonomy lists which postures this agent can actually run in.
	Autonomy AutonomyLadder `json:"autonomy"`
}

// AutonomyLadder is the set of modes the server can enforce, with the reason for
// any that are missing.
type AutonomyLadder struct {
	// Available lists the selectable modes, narrowest first.
	Available []string `json:"available"`
	// Default is what an unset mode becomes.
	Default string `json:"default"`
	// GuidedUnavailableReason is set when guided is absent from Available.
	GuidedUnavailableReason string `json:"guided_unavailable_reason,omitempty"`
}

// Supervisor owns the turn lifecycle for one sandbox.
type Supervisor struct {
	// GuidedAvailable reports whether a working approval gate exists. Nil means the
	// agent has no verified permission-tool flag at all.
	GuidedAvailable func() bool
	// GateConsulted reports the cumulative number of gate decisions; nil when there is no gate.
	GateConsulted func() int

	Queue   *Queue
	Runner  *Runner
	Journal *Journal
	// DefaultAutonomy is the launch-time posture for turns that name no mode. Empty
	// means Autonomous. A mode the ladder does not offer is ignored, not obeyed.
	DefaultAutonomy string

	// Mode reports how much of the agent's work is visible.
	Mode Mode
	// Budget caps reported model spend. Nil means no cap is enforced.
	Budget       *Budget
	AgentVersion string
	PollInterval time.Duration
	Now          func() time.Time

	mu       sync.Mutex
	state    SessionState
	reason   string
	restarts int
	// guidedRevoked is sticky for the session: a gate bypassed once has disproved itself.
	guidedRevoked bool
	guidedReason  string

	// wake is buffered depth 1 so many enqueues collapse into one drain pass.
	wakeOnce sync.Once
	wake     chan struct{}
}

// init must not touch s.state: Notify calls it from submitting goroutines.
func (s *Supervisor) init() {
	s.wakeOnce.Do(func() { s.wake = make(chan struct{}, 1) })
}

func (s *Supervisor) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return defaultPollInterval
}

// Notify tells the loop there may be work. Non-blocking.
func (s *Supervisor) Notify() {
	s.init()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run drains the queue until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	s.init()

	_, _ = s.Journal.Append(EventSessionStarting, "", map[string]any{
		"mode": string(s.Mode), "agent_version": s.AgentVersion,
	})
	s.setState(StateIdle, "")
	_, _ = s.Journal.Append(EventSessionReady, "", map[string]any{
		"mode": string(s.Mode), "agent_version": s.AgentVersion,
	})

	ticker := time.NewTicker(s.pollInterval())
	defer ticker.Stop()

	for {
		// Drain fully before waiting so queued turns do not each need a wake.
		for {
			ran, err := s.step(ctx)
			if err != nil {
				return err
			}
			if !ran {
				break
			}
		}

		select {
		case <-ctx.Done():
			s.setState(StateStopped, "")
			_, _ = s.Journal.Append(EventSessionStopped, "", nil)
			_ = s.Journal.Sync()
			return nil
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

// step runs at most one turn and reports whether it ran one.
func (s *Supervisor) step(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, nil
	}
	// Freeze before claiming; the turn stays queued so raising the cap lets it run.
	if s.Budget != nil && s.Budget.Exceeded() {
		s.degrade(spendFrozenReason)
		return false, nil
	}

	turn, err := s.Queue.Claim()
	if err != nil {
		// Degrade instead of spinning; the next tick retries.
		s.degrade("could not claim the next task: " + err.Error())
		return false, nil
	}
	if turn == nil {
		s.setState(StateIdle, "")
		return false, nil
	}

	s.setState(StateBusy, "")
	consultedBefore := 0
	if s.GateConsulted != nil {
		consultedBefore = s.GateConsulted()
	}

	res, runErr := s.Runner.Run(ctx, turn)
	seqEnd := s.Journal.LastSeq()

	s.auditGateConsultation(turn, res, consultedBefore)

	// Record session id and reply before Finish so a completed-turn read does not lag.
	_ = s.Queue.SetSessionID(turn.ID, res.SessionID)
	_ = s.Queue.SetResponse(turn.ID, res.Reply)
	s.recordSpend(turn.ID, res.CostUSD)

	switch {
	case runErr != nil:
		// The turn could not start at all; keep serving, this is usually user-fixable.
		_ = s.Queue.Finish(turn.ID, TurnFailed, nil, runErr.Error(), seqEnd)
		s.degrade("the last task could not be started: " + runErr.Error())
	case res.Interrupted:
		code := res.ExitCode
		_ = s.Queue.Finish(turn.ID, TurnInterrupted, &code, "", seqEnd)
		s.setState(StateIdle, "")
	case res.ExitCode != 0:
		code := res.ExitCode
		// A non-zero agent exit is information, not a degraded session.
		_ = s.Queue.Finish(turn.ID, TurnFailed, &code, "", seqEnd)
		s.setState(StateIdle, "")
	default:
		code := 0
		_ = s.Queue.Finish(turn.ID, TurnDone, &code, "", seqEnd)
		s.setState(StateIdle, "")
	}
	return true, nil
}

// spendFrozenReason is the user-facing degraded reason when the cap stops the queue.
const spendFrozenReason = "the spend cap for this workstation has been reached, so queued tasks are " +
	"paused; raise or clear the cap to continue"

// recordSpend accounts for a finished turn and announces any threshold crossed.
func (s *Supervisor) recordSpend(turnID string, costUSD *float64) {
	if s.Budget == nil {
		return
	}
	crossed := s.Budget.Record(costUSD)
	snap := s.Budget.Snapshot()
	for _, pct := range crossed {
		_, _ = s.Journal.Append(EventSpendThreshold, turnID, map[string]any{
			"percent": pct, "spent_usd": snap.SpentUSD, "cap_usd": snap.CapUSD,
			"turns_unreported": snap.TurnsUnreported, "coverage": snap.Coverage,
		})
	}
	if snap.Exceeded {
		s.degrade(spendFrozenReason)
	}
}

// Submit enqueues a turn, journals it as queued, and nudges the loop. The queued
// event is written here so a client streaming from SeqStart sees the ack immediately.
func (s *Supervisor) Submit(req, mode, thread, source string, priority bool) (*Turn, error) {
	if s.Budget != nil && s.Budget.Exceeded() {
		return nil, ErrSpendCapReached
	}
	// Refuse rather than downgrade: Templates.Build has no guided-without-a-gate
	// case, so the turn would run as a plain command with an unattended approval prompt.
	if err := s.checkMode(mode); err != nil {
		return nil, err
	}
	turn, err := s.Queue.Enqueue(req, mode, thread, source, priority)
	if err != nil {
		return nil, err
	}
	ev, jerr := s.Journal.Append(EventTurnQueued, turn.ID, map[string]any{
		"request": req, "mode": mode, "source": source, "priority": priority,
	})
	if jerr == nil {
		// Enqueue returns a clone, so persist the cursor too. A failure here only
		// loses the transcript cursor; the turn still runs.
		turn.SeqStart = ev.Seq
		_ = s.Queue.SetSeqStart(turn.ID, ev.Seq)
	}
	s.Notify()
	return turn, nil
}

// Interrupt stops the running turn.
func (s *Supervisor) Interrupt(turnID string, grace time.Duration) error {
	return s.Runner.Interrupt(turnID, grace)
}

// SafeToSnapshot reports whether the sandbox can be paused without capturing a
// half-applied tool write.
func (s *Supervisor) SafeToSnapshot() (bool, string) {
	if s.Runner != nil && s.Runner.Busy() {
		return false, "a task is running; pausing now could capture a half-finished file write"
	}
	return true, ""
}

// Status reports the session for GET /agent/status.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	state, reason, restarts := s.state, s.reason, s.restarts
	s.mu.Unlock()

	if state == "" {
		state = StateStarting
	}
	st := Status{
		State: state, Mode: s.Mode, DegradedReason: reason,
		CurrentTurn: s.Queue.Running(), QueueDepth: s.Queue.Depth(),
		LastSeq: s.Journal.LastSeq(), Restarts: restarts,
		AgentVersion: s.AgentVersion,
		Autonomy:     s.autonomy(),
	}
	if s.Budget != nil {
		snap := s.Budget.Snapshot()
		st.Spend = &snap
	}
	return st
}

func (s *Supervisor) setState(state SessionState, reason string) {
	s.mu.Lock()
	// Degraded is sticky until Recovered clears it; one good turn proves nothing.
	if s.state == StateDegraded && state == StateIdle {
		s.mu.Unlock()
		return
	}
	s.state, s.reason = state, reason
	s.mu.Unlock()
}

// degrade records a capability loss and journals it once per distinct reason.
func (s *Supervisor) degrade(reason string) {
	s.mu.Lock()
	already := s.state == StateDegraded && s.reason == reason
	s.state, s.reason = StateDegraded, reason
	s.mu.Unlock()

	if !already {
		_, _ = s.Journal.Append(EventSessionDegraded, "", map[string]any{"reason": reason})
	}
}

// Recovered clears a degraded state once the caller knows the cause is gone.
func (s *Supervisor) Recovered() {
	s.mu.Lock()
	wasDegraded := s.state == StateDegraded
	s.state, s.reason = StateIdle, ""
	s.mu.Unlock()

	if wasDegraded {
		_, _ = s.Journal.Append(EventSessionReady, "", map[string]any{
			"mode": string(s.Mode), "recovered": true,
		})
	}
}

// NoteAgentRestart records that the agent process was replaced after an unexpected exit.
func (s *Supervisor) NoteAgentRestart(reason string) {
	s.mu.Lock()
	s.restarts++
	n := s.restarts
	s.mu.Unlock()

	// The count is part of the reason so each restart is journaled separately.
	s.degrade(fmt.Sprintf("the agent process stopped unexpectedly and was restarted (%d so far): %s", n, reason))
}

// autonomy reports the modes this agent can genuinely run in. Supervised and
// Autonomous are always offered; Guided needs a verified gate.
func (s *Supervisor) autonomy() AutonomyLadder {
	l := AutonomyLadder{
		Available: []string{ModeSupervised},
		Default:   ModeAutonomous,
	}
	if s.guidedRevokedNow() {
		l.GuidedUnavailableReason = s.guidedRevokedReason()
		l.Available = append(l.Available, ModeAutonomous)
		l.Default = s.defaultAutonomy(l.Available)
		return l
	}
	switch {
	case s.GuidedAvailable == nil:
		l.GuidedUnavailableReason = "this agent has no verified approval gate, so tool calls " +
			"cannot be intercepted for review"
	case !s.GuidedAvailable():
		l.GuidedUnavailableReason = "the approval gate could not be started in this sandbox, " +
			"so Guided mode is not being offered rather than running unreviewed"
	default:
		l.Available = append(l.Available, ModeGuided)
	}
	l.Available = append(l.Available, ModeAutonomous)
	l.Default = s.defaultAutonomy(l.Available)
	return l
}

// defaultAutonomy resolves the launch-time choice against what is available,
// falling back to Autonomous.
func (s *Supervisor) defaultAutonomy(available []string) string {
	want := s.DefaultAutonomy
	if want == "" {
		return ModeAutonomous
	}
	for _, m := range available {
		if m == want {
			return want
		}
	}
	return ModeAutonomous
}

// ErrModeUnavailable is returned when a caller asks for a mode this agent cannot run in.
var ErrModeUnavailable = errors.New("this agent cannot run in that mode")

// checkMode rejects a mode the ladder does not offer. Empty is fine (Enqueue
// defaults it); an unknown mode is a typo in a safety setting, so refuse it.
func (s *Supervisor) checkMode(mode string) error {
	if mode == "" {
		return nil
	}
	for _, m := range s.autonomy().Available {
		if m == mode {
			return nil
		}
	}
	if mode == ModeGuided {
		return fmt.Errorf("%w: %s", ErrModeUnavailable, s.autonomy().GuidedUnavailableReason)
	}
	return fmt.Errorf("%w: %q is not one of %v", ErrModeUnavailable, mode, s.autonomy().Available)
}

func (s *Supervisor) guidedRevokedNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guidedRevoked
}

func (s *Supervisor) guidedRevokedReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guidedReason
}

// auditGateConsultation revokes Guided for the session when a Guided turn used
// tools without the gate being consulted, which means the permission flag is
// ignored. ToolCalls is only visible in the structured stream, so basic-mode turns are skipped.
func (s *Supervisor) auditGateConsultation(turn *Turn, res Result, consultedBefore int) {
	if turn.Mode != ModeGuided || s.GateConsulted == nil {
		return
	}
	if res.ToolCalls == 0 {
		return
	}
	if s.GateConsulted() > consultedBefore {
		return
	}

	reason := "the agent used tools in Guided mode without consulting the approval gate, " +
		"so its permission flag is being ignored by this build of the CLI; Guided mode is " +
		"withdrawn for this session rather than run unreviewed"
	s.mu.Lock()
	already := s.guidedRevoked
	s.guidedRevoked, s.guidedReason = true, reason
	s.mu.Unlock()
	if already {
		return
	}
	_, _ = s.Journal.Append(EventSessionDegraded, turn.ID, map[string]any{
		"reason": reason, "tool_calls": res.ToolCalls, "gate_consultations": 0,
	})
	s.degrade(reason)
}
