package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Exactly one turn runs at a time: agent CLIs keep their transcript on disk and
// read it on resume, so concurrent turns would corrupt the conversation.

// ErrQueueFull is returned when the pending queue is at capacity.
var ErrQueueFull = errors.New("the workstation already has as many queued tasks as it will hold")

// ErrNotFound is returned for an unknown turn id.
var ErrNotFound = errors.New("no such turn")

// ErrSpendCapReached is returned when the spend cap has stopped work. Surfaced as
// 402, not 429, since retrying does not help.
var ErrSpendCapReached = errors.New("the spend cap for this workstation has been reached; " +
	"raise or clear the cap to continue")

// defaultMaxPending bounds the queue.
const defaultMaxPending = 10

// Turn is one unit of agent work, and the durable record of what happened to it.
type Turn struct {
	ID      string    `json:"id"`
	Request string    `json:"request"`
	State   TurnState `json:"state"`
	// Mode is the autonomy posture: supervised, guided, or autonomous.
	Mode string `json:"mode"`
	// Thread selects the conversation: "latest", "new", or a specific id.
	Thread string `json:"thread,omitempty"`
	// FollowUp is decided at enqueue time and persisted so a restart does not change it.
	FollowUp bool `json:"follow_up,omitempty"`
	// Source records where the turn came from (dashboard, api, telegram, cron).
	Source string `json:"source,omitempty"`
	// Ordinal is the queue position, kept in the record so a reorder rewrites data
	// instead of renaming files.
	Ordinal uint64 `json:"ordinal"`
	// Priority jumps the queue; ties break by ordinal.
	Priority bool `json:"priority,omitempty"`

	QueuedAt  time.Time  `json:"queued_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	// Response is the agent's answer, persisted so reads need not replay the journal.
	Response string `json:"response,omitempty"`
	// SessionID is the agent CLI's own conversation id, from the structured stream.
	SessionID string `json:"session_id,omitempty"`
	// SeqStart is the journal cursor a client streams this turn from.
	SeqStart uint64 `json:"seq_start,omitempty"`
	SeqEnd   uint64 `json:"seq_end,omitempty"`
}

// Queue is a durable FIFO of turns, with at most one running.
type Queue struct {
	dir        string
	maxPending int
	now        func() time.Time

	mu      sync.Mutex
	turns   map[string]*Turn
	ordinal uint64
}

// OpenQueue opens (or creates) the queue in dir and recovers what was pending.
func OpenQueue(dir string) (*Queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("queue dir: %w", err)
	}
	q := &Queue{
		dir: dir, maxPending: defaultMaxPending, now: time.Now,
		turns: map[string]*Turn{},
	}
	if err := q.load(); err != nil {
		return nil, err
	}
	return q, nil
}

// load reads every turn record back. A turn that was running is marked failed,
// not re-queued: it may already have had effects a retry would repeat.
func (q *Queue) load() error {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(q.dir, e.Name()))
		if err != nil {
			return fmt.Errorf("read turn %s: %w", e.Name(), err)
		}
		var t Turn
		if err := json.Unmarshal(b, &t); err != nil {
			// Skip a torn record rather than wedge the whole workstation.
			continue
		}
		if t.ID == "" {
			continue
		}
		if t.State == TurnRunning || t.State == TurnAwaiting {
			t.State = TurnFailed
			t.Error = "the supervisor restarted while this turn was running, so it was stopped part-way; " +
				"any changes it had already made are still there"
			at := q.now().UTC()
			t.EndedAt = &at
			// Best effort; a failed write is re-resolved the same way next load.
			_ = q.write(&t)
		}
		if t.Ordinal > q.ordinal {
			q.ordinal = t.Ordinal
		}
		q.turns[t.ID] = &t
	}
	return nil
}

func (q *Queue) path(id string) string { return filepath.Join(q.dir, id+".json") }

// write persists a turn atomically via temp file and rename.
func (q *Queue) write(t *Turn) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := q.path(t.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path(t.ID))
}

// NewTurnID returns a sortable, unguessable turn id: millisecond timestamp plus random suffix.
func NewTurnID(now time.Time) string {
	var r [5]byte
	_, _ = rand.Read(r[:])
	return fmt.Sprintf("turn_%012x%s", now.UTC().UnixMilli(), hex.EncodeToString(r[:]))
}

// Autonomy modes.
const (
	// ModeAutonomous skips the agent's own tool-approval gate; the sandbox is the boundary.
	ModeAutonomous = "autonomous"

	// ModeGuided routes every tool call through the approval gate; unanswered calls
	// are denied. Offered only when Templates.GuidedAvailable verifies the gate works.
	ModeGuided = "guided"

	// ModeSupervised runs nothing on its own.
	ModeSupervised = "supervised"
)

// Enqueue adds a turn and returns a copy. An unset mode becomes autonomous:
// otherwise the agent's own approval prompt stays armed with nobody at it and
// silently refuses every write while the turn still reports done.
func (q *Queue) Enqueue(req, mode, thread, source string, priority bool) (*Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if mode == "" {
		mode = ModeAutonomous
	}

	if n := q.countPendingLocked(); n >= q.maxPending {
		return nil, ErrQueueFull
	}
	now := q.now().UTC()
	q.ordinal++
	t := &Turn{
		ID: NewTurnID(now), Request: req, State: TurnQueued,
		Mode: mode, Thread: thread, Source: source,
		Ordinal: q.ordinal, Priority: priority, QueuedAt: now,
		FollowUp: thread != "new" && q.hasHistoryLocked(),
	}
	if err := q.write(t); err != nil {
		return nil, fmt.Errorf("persist turn: %w", err)
	}
	q.turns[t.ID] = t
	return t.clone(), nil
}

// hasHistoryLocked reports whether any turn has run; queued-only turns do not count.
func (q *Queue) hasHistoryLocked() bool {
	for _, t := range q.turns {
		if t.State.Terminal() || t.State == TurnRunning || t.State == TurnAwaiting {
			return true
		}
	}
	return false
}

func (q *Queue) countPendingLocked() int {
	n := 0
	for _, t := range q.turns {
		if t.State == TurnQueued {
			n++
		}
	}
	return n
}

// pendingLocked returns queued turns in service order: priority first, then ordinal.
func (q *Queue) pendingLocked() []*Turn {
	var out []*Turn
	for _, t := range q.turns {
		if t.State == TurnQueued {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority
		}
		return out[i].Ordinal < out[j].Ordinal
	})
	return out
}

// Claim marks the next queued turn as running and returns it, or nil when nothing
// is queued or a turn is already running.
func (q *Queue) Claim() (*Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, t := range q.turns {
		if t.State == TurnRunning || t.State == TurnAwaiting {
			return nil, nil
		}
	}
	pending := q.pendingLocked()
	if len(pending) == 0 {
		return nil, nil
	}
	t := pending[0]
	at := q.now().UTC()
	t.State, t.StartedAt = TurnRunning, &at
	if err := q.write(t); err != nil {
		// Roll back so memory agrees with disk and the turn is retried.
		t.State, t.StartedAt = TurnQueued, nil
		return nil, fmt.Errorf("claim turn: %w", err)
	}
	return t.clone(), nil
}

// SetSessionID records the agent's own conversation id for a turn.
func (q *Queue) SetSessionID(id, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.turns[id]
	if !ok {
		return ErrNotFound
	}
	if t.SessionID == sessionID {
		return nil
	}
	t.SessionID = sessionID
	return q.write(t)
}

// Threads groups turns by the agent's conversation id, newest activity first.
// Turns with no reported session id are grouped under the empty id.
func (q *Queue) Threads() []Thread {
	q.mu.Lock()
	defer q.mu.Unlock()

	byID := map[string]*Thread{}
	for _, t := range q.turns {
		th, ok := byID[t.SessionID]
		if !ok {
			th = &Thread{ID: t.SessionID}
			byID[t.SessionID] = th
		}
		th.Turns++
		if th.FirstAt.IsZero() || t.QueuedAt.Before(th.FirstAt) {
			th.FirstAt = t.QueuedAt
		}
		if t.QueuedAt.After(th.LastAt) {
			th.LastAt = t.QueuedAt
			th.Title = truncateRunes(t.Request, 80)
		}
	}
	out := make([]Thread, 0, len(byID))
	for _, th := range byID {
		out = append(out, *th)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out
}

// Thread is one conversation on this workstation.
type Thread struct {
	// ID is the agent CLI's own conversation id; empty when it never reported one.
	ID      string    `json:"id"`
	Title   string    `json:"title,omitempty"`
	Turns   int       `json:"turns"`
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
}

// TurnsForThread returns a thread's turns, newest first.
func (q *Queue) TurnsForThread(sessionID string) []*Turn {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*Turn
	for _, t := range q.turns {
		if t.SessionID == sessionID {
			out = append(out, t.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal > out[j].Ordinal })
	return out
}

// truncateRunes bounds a label without splitting a multi-byte character.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// SetResponse records the agent's answer for a turn.
func (q *Queue) SetResponse(id, response string) error {
	if response == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.turns[id]
	if !ok {
		return ErrNotFound
	}
	t.Response = response
	return q.write(t)
}

// Finish records a terminal state for a turn.
func (q *Queue) Finish(id string, state TurnState, exitCode *int, errMsg string, seqEnd uint64) error {
	if !state.Terminal() {
		return fmt.Errorf("finish requires a terminal state, got %q", state)
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	t, ok := q.turns[id]
	if !ok {
		return ErrNotFound
	}
	at := q.now().UTC()
	t.State, t.EndedAt, t.ExitCode, t.Error, t.SeqEnd = state, &at, exitCode, errMsg, seqEnd
	return q.write(t)
}

// SetSeqStart durably records the journal cursor a turn begins at.
func (q *Queue) SetSeqStart(id string, seq uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	t, ok := q.turns[id]
	if !ok {
		return ErrNotFound
	}
	// Forward-only, or a client that already streamed from it would replay events.
	if seq <= t.SeqStart {
		return nil
	}
	t.SeqStart = seq
	return q.write(t)
}

// SetState moves a turn between non-terminal states.
func (q *Queue) SetState(id string, state TurnState) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.turns[id]
	if !ok {
		return ErrNotFound
	}
	t.State = state
	return q.write(t)
}

// Remove dequeues a turn that has not started; a running turn must be interrupted instead.
func (q *Queue) Remove(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	t, ok := q.turns[id]
	if !ok {
		return ErrNotFound
	}
	if t.State != TurnQueued {
		return fmt.Errorf("turn %s is %s, not queued — interrupt it instead", id, t.State)
	}
	if err := os.Remove(q.path(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(q.turns, id)
	return nil
}

// Reorder sets the service order of the pending queue. Ids not mentioned keep
// their relative order behind the ones that were.
func (q *Queue) Reorder(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	pending := q.pendingLocked()
	byID := make(map[string]*Turn, len(pending))
	for _, t := range pending {
		byID[t.ID] = t
	}
	var ordered []*Turn
	seen := map[string]bool{}
	for _, id := range ids {
		t, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: %s is not queued", ErrNotFound, id)
		}
		if seen[id] {
			return fmt.Errorf("%s listed twice", id)
		}
		seen[id] = true
		ordered = append(ordered, t)
	}
	for _, t := range pending {
		if !seen[t.ID] {
			ordered = append(ordered, t)
		}
	}
	// Renumber from the high-water mark so ordinals stay monotonic.
	for _, t := range ordered {
		q.ordinal++
		t.Ordinal, t.Priority = q.ordinal, false
		if err := q.write(t); err != nil {
			return err
		}
	}
	return nil
}

// Get returns a copy of one turn.
func (q *Queue) Get(id string) (*Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.turns[id]
	if !ok {
		return nil, ErrNotFound
	}
	return t.clone(), nil
}

// List returns every turn, newest first.
func (q *Queue) List() []*Turn {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*Turn, 0, len(q.turns))
	for _, t := range q.turns {
		out = append(out, t.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal > out[j].Ordinal })
	return out
}

// Pending returns the queued turns in service order.
func (q *Queue) Pending() []*Turn {
	q.mu.Lock()
	defer q.mu.Unlock()
	src := q.pendingLocked()
	out := make([]*Turn, 0, len(src))
	for _, t := range src {
		out = append(out, t.clone())
	}
	return out
}

// Running returns the turn currently executing or parked for approval, if any.
func (q *Queue) Running() *Turn {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.turns {
		if t.State == TurnRunning || t.State == TurnAwaiting {
			return t.clone()
		}
	}
	return nil
}

// Depth is the number of turns waiting.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.countPendingLocked()
}

// clone returns a deep copy so callers cannot mutate queue state.
func (t *Turn) clone() *Turn {
	cp := *t
	if t.StartedAt != nil {
		v := *t.StartedAt
		cp.StartedAt = &v
	}
	if t.EndedAt != nil {
		v := *t.EndedAt
		cp.EndedAt = &v
	}
	if t.ExitCode != nil {
		v := *t.ExitCode
		cp.ExitCode = &v
	}
	return &cp
}
