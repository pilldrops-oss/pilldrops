package agentd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd/parse"
)

const (
	// Output is flushed on a timer or a full buffer so event count tracks duration, not verbosity.
	outputFlushInterval = 100 * time.Millisecond
	outputFlushBytes    = 4 << 10

	// maxStructuredLine bounds one JSONL frame; frames carry file contents, so the
	// scanner default of 64 KiB would truncate them into raw text.
	maxStructuredLine = 8 << 20

	// defaultGracePeriod lets an interrupted agent flush the transcript that the
	// next turn's --continue reads before it is killed.
	defaultGracePeriod = 5 * time.Second
)

// ErrNoTurnRunning is returned when an interrupt arrives with nothing to stop.
var ErrNoTurnRunning = errors.New("no turn is running")

// CommandBuilder produces the shell command and environment for a turn.
type CommandBuilder func(t *Turn) (cmd string, env []string, err error)

// Result is how a turn ended.
type Result struct {
	ExitCode    int
	Interrupted bool
	// Reply is the bounded tail of stdout; the full output is in the journal.
	Reply string
	// SessionID is the agent's own conversation id, empty for a basic-mode agent.
	SessionID string
	// CostUSD is the model cost the agent reported. Nil means unknown, not free.
	CostUSD *float64
	// ToolCalls counts tool invocations seen in the structured stream. Zero proves
	// nothing for a basic-mode agent, which emits no tool frames.
	ToolCalls int
}

// maxReplyBytes bounds the in-memory reply.
const maxReplyBytes = 64 << 10

// Runner executes turns, one at a time.
type Runner struct {
	Journal *Journal
	Build   CommandBuilder
	// Shell runs the command string, which carries quoting and env prefixes.
	Shell string
	Dir   string
	Grace time.Duration
	// Parser reads the agent's structured event stream. Nil means plain capture.
	Parser parse.Parser
	// Structured reports whether this turn emits structured events. Per turn, since
	// a follow-up on an agent without a verified stream+continue command runs plain.
	Structured func(*Turn) bool
	// OnTurnStart is called before spawn so the approval gate can attribute parked
	// calls to the current turn.
	OnTurnStart func(turnID string)

	mu     sync.Mutex
	active *exec.Cmd
	turnID string
	// interrupted lets the exit be reported as an interrupt rather than a non-zero exit.
	interrupted bool
}

func (r *Runner) shell() string {
	if r.Shell != "" {
		return r.Shell
	}
	return "/bin/sh"
}

func (r *Runner) grace() time.Duration {
	if r.Grace > 0 {
		return r.Grace
	}
	return defaultGracePeriod
}

// Run executes a turn to completion and journals its lifecycle. It returns an
// error only when the turn could not be run at all; a non-zero exit is a Result.
func (r *Runner) Run(ctx context.Context, t *Turn) (Result, error) {
	cmdStr, env, err := r.Build(t)
	if err != nil {
		_, _ = r.Journal.Append(EventTurnFailed, t.ID, map[string]any{"error": err.Error()})
		return Result{}, err
	}

	if _, err := r.Journal.Append(EventTurnStarted, t.ID, map[string]any{
		"request": t.Request, "mode": t.Mode, "source": t.Source,
	}); err != nil {
		return Result{}, fmt.Errorf("journal turn start: %w", err)
	}

	if r.OnTurnStart != nil {
		r.OnTurnStart(t.ID)
	}

	cmd := exec.CommandContext(ctx, r.shell(), "-c", cmdStr)
	cmd.Dir = r.Dir
	cmd.Env = env
	setProcessGroup(cmd)
	// The default Cancel SIGKILLs only the shell, orphaning the agent's children.
	cmd.Cancel = func() error { return r.stop(r.grace()) }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}

	if err := cmd.Start(); err != nil {
		_, _ = r.Journal.Append(EventTurnFailed, t.ID, map[string]any{"error": err.Error()})
		return Result{}, fmt.Errorf("start agent: %w", err)
	}

	r.mu.Lock()
	r.active, r.turnID, r.interrupted = cmd, t.ID, false
	r.mu.Unlock()

	var (
		wg    sync.WaitGroup
		reply replyBuffer
	)
	acc := newTurnAccum()
	structured := r.Parser != nil && r.Structured != nil && r.Structured(t)

	wg.Add(2)
	go func() {
		defer wg.Done()
		if structured {
			r.pumpStructured(t.ID, stdout, &reply, acc)
			return
		}
		r.pump(t.ID, "stdout", stdout, &reply)
	}()
	// stderr is never JSONL, so it always takes the byte pump.
	go func() { defer wg.Done(); r.pump(t.ID, "stderr", stderr, nil) }()
	// cmd.Wait closes the pipes, so drain both pumps first.
	wg.Wait()
	waitErr := cmd.Wait()

	r.mu.Lock()
	wasInterrupted := r.interrupted
	r.active, r.turnID, r.interrupted = nil, "", false
	r.mu.Unlock()

	exitCode := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else if !wasInterrupted {
			_, _ = r.Journal.Append(EventTurnFailed, t.ID, map[string]any{"error": waitErr.Error()})
			_ = r.Journal.FinishTurn(t.ID)
			return Result{}, waitErr
		}
	}

	res := Result{
		ExitCode: exitCode, Interrupted: wasInterrupted, Reply: reply.String(),
		SessionID: acc.session(), CostUSD: acc.cost(),
		ToolCalls: acc.toolCallCount(),
	}
	switch {
	case wasInterrupted:
		_, _ = r.Journal.Append(EventTurnInterrupted, t.ID, map[string]any{
			"exit_code": exitCode,
			"note":      "stopped on request; output produced before stopping is kept",
		})
	case exitCode != 0:
		_, _ = r.Journal.Append(EventTurnFailed, t.ID,
			acc.finishPayload(map[string]any{"exit_code": exitCode}))
	default:
		_, _ = r.Journal.Append(EventTurnFinished, t.ID,
			acc.finishPayload(map[string]any{"exit_code": 0}))
	}
	_ = r.Journal.FinishTurn(t.ID)
	return res, nil
}

// turnAccum is the per-turn state the structured pump keeps.
type turnAccum struct {
	mu        sync.Mutex
	step      int
	toolStart map[string]time.Time
	usage     *parse.Usage
	sessionID string
	// textTail is the recent visible output, used to skip a result frame that
	// restates text the user already watched stream.
	textTail []byte
	// toolCalls counts tool_use frames seen this turn.
	toolCalls int
}

func newTurnAccum() *turnAccum {
	return &turnAccum{toolStart: map[string]time.Time{}}
}

func (a *turnAccum) noteToolCall() {
	a.mu.Lock()
	a.toolCalls++
	a.mu.Unlock()
}

func (a *turnAccum) toolCallCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolCalls
}

func (a *turnAccum) nextStep() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.step++
	return a.step
}

// openTool records when a tool call started so its result can report a duration.
func (a *turnAccum) openTool(id string, at time.Time) {
	if id == "" {
		return
	}
	a.mu.Lock()
	a.toolStart[id] = at
	a.mu.Unlock()
}

// closeTool returns how long a tool ran, and whether its start was seen.
func (a *turnAccum) closeTool(id string, at time.Time) (time.Duration, bool) {
	if id == "" {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	started, ok := a.toolStart[id]
	if !ok {
		return 0, false
	}
	delete(a.toolStart, id)
	return at.Sub(started), true
}

// cost reports the model cost the agent declared, or nil if it declared none.
func (a *turnAccum) cost() *float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usage == nil || a.usage.CostUSD == nil {
		return nil
	}
	v := *a.usage.CostUSD
	return &v
}

// session reports the agent's conversation id, if one was seen.
func (a *turnAccum) session() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

// streamedTailMax bounds the output retained for the duplicate check.
const streamedTailMax = 64 << 10

// noteText records visible output that reached the transcript.
func (a *turnAccum) noteText(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.textTail = append(a.textTail, text...)
	if len(a.textTail) > streamedTailMax {
		a.textTail = a.textTail[len(a.textTail)-streamedTailMax:]
	}
}

// alreadyStreamed reports whether text has already been shown this turn. When
// unsure it says no, since a duplicate is cosmetic and a drop is lost work.
func (a *turnAccum) alreadyStreamed(text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := strings.TrimSpace(text)
	if t == "" || len(t) > len(a.textTail) {
		return false
	}
	return strings.Contains(string(a.textTail), t)
}

func (a *turnAccum) setUsage(u parse.Usage) {
	a.mu.Lock()
	a.usage = &u
	a.mu.Unlock()
}

func (a *turnAccum) setSession(id string) {
	if id == "" {
		return
	}
	a.mu.Lock()
	a.sessionID = id
	a.mu.Unlock()
}

// finishPayload adds reported usage to the terminal event, omitting absent numbers
// rather than sending zero.
func (a *turnAccum) finishPayload(base map[string]any) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessionID != "" {
		base["session_id"] = a.sessionID
	}
	if a.usage == nil {
		return base
	}
	if a.usage.InputTokens > 0 {
		base["input_tokens"] = a.usage.InputTokens
	}
	if a.usage.OutputTokens > 0 {
		base["output_tokens"] = a.usage.OutputTokens
	}
	if a.usage.CacheReadTokens > 0 {
		base["cache_read_tokens"] = a.usage.CacheReadTokens
	}
	if a.usage.CostUSD != nil {
		base["model_cost_usd"] = *a.usage.CostUSD
	}
	return base
}

// pumpStructured reads a JSONL event stream line by line and journals each step.
// Unrecognised frames are journaled as plain output rather than dropped.
func (r *Runner) pumpStructured(turnID string, rc io.ReadCloser, reply *replyBuffer, acc *turnAccum) {
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), maxStructuredLine)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// The scanner reuses its buffer.
		cp := make([]byte, len(line))
		copy(cp, line)
		r.journalStep(turnID, r.Parser.Parse(cp), reply, acc)
	}
	if err := sc.Err(); err != nil {
		_, _ = r.Journal.Append(EventTurnOutput, turnID, map[string]any{
			"text": "(the agent's event stream ended early: " + err.Error() + ")", "stream": "stdout",
		})
	}
}

// journalStep turns one parsed step into journal events.
func (r *Runner) journalStep(turnID string, st parse.Step, reply *replyBuffer, acc *turnAccum) {
	now := time.Now()

	switch st.Kind {
	case parse.KindSession:
		acc.setSession(st.SessionID)
		_, _ = r.Journal.Append(EventSessionConfig, turnID, map[string]any{
			"session_id": st.SessionID, "model": st.Model, "tools": st.Tools,
			"mcp_servers": st.MCPServers, "permission_mode": st.PermissionMode,
			"agent_version": st.AgentVersion,
		})

	case parse.KindText:
		if reply != nil {
			reply.Write([]byte(st.Text))
		}
		acc.noteText(st.Text)
		_, _ = r.Journal.AppendOutput(turnID, st.Text)

	case parse.KindThinking:
		// claude-code emits an empty thinking block when the API redacts reasoning.
		if st.Text == "" {
			break
		}
		_, _ = r.Journal.AppendStep(EventTurnThinking, turnID, acc.nextStep(),
			map[string]any{"text": st.Text})

	case parse.KindToolStart:
		acc.noteToolCall()
		acc.openTool(st.ToolID, now)
		_, _ = r.Journal.AppendStep(EventToolStarted, turnID, acc.nextStep(), map[string]any{
			"tool": st.Tool, "tool_id": st.ToolID, "detail": st.Detail,
		})

	case parse.KindToolEnd, parse.KindToolFail:
		typ := EventToolFinished
		if st.Kind == parse.KindToolFail {
			typ = EventToolFailed
		}
		// The tool_result frame carries the id but not the name; omit rather than send "".
		payload := map[string]any{"tool_id": st.ToolID}
		if st.Tool != "" {
			payload["tool"] = st.Tool
		}
		if st.Detail != "" {
			payload["detail"] = st.Detail
		}
		if st.Text != "" {
			payload["result"] = st.Text
		}
		if d, ok := acc.closeTool(st.ToolID, now); ok {
			payload["ms"] = d.Milliseconds()
		}
		_, _ = r.Journal.AppendStep(typ, turnID, acc.nextStep(), payload)

	case parse.KindResult:
		acc.setUsage(st.Usage)
		for _, d := range st.Denials {
			_, _ = r.Journal.AppendStep(EventApprovalDenied, turnID, acc.nextStep(),
				map[string]any{"tool": d.Tool, "tool_id": d.ToolID, "detail": d.Detail})
		}
		// The result frame restates the final answer; skip it if it already streamed.
		if st.Text != "" && !acc.alreadyStreamed(st.Text) {
			if reply != nil {
				reply.Write([]byte(st.Text))
			}
			_, _ = r.Journal.AppendOutput(turnID, st.Text)
		}

	case parse.KindError:
		if reply != nil {
			reply.Write([]byte(st.Text))
		}
		_, _ = r.Journal.AppendStep(EventTurnOutput, turnID, 0, map[string]any{
			"text": st.Text, "stream": "stderr",
		})

	default:
		if reply != nil {
			reply.Write(st.Raw)
		}
		_, _ = r.Journal.AppendOutput(turnID, string(st.Raw))
	}
}

// pump reads a stream, coalesces it, and journals it. The flush runs on a ticker,
// not on the next read, so a line printed before minutes of silence is not held back.
func (r *Runner) pump(turnID, stream string, rc io.ReadCloser, reply *replyBuffer) {
	defer rc.Close()

	var (
		mu  sync.Mutex
		buf []byte
	)
	flush := func() {
		mu.Lock()
		if len(buf) == 0 {
			mu.Unlock()
			return
		}
		text := string(buf)
		buf = nil
		mu.Unlock()
		_, _ = r.Journal.AppendStep(EventTurnOutput, turnID, 0, map[string]any{
			"text": text, "stream": stream,
		})
	}

	stopTick := make(chan struct{})
	var ticker sync.WaitGroup
	ticker.Add(1)
	go func() {
		defer ticker.Done()
		t := time.NewTicker(outputFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				flush()
			case <-stopTick:
				return
			}
		}
	}()

	readBuf := make([]byte, 32<<10)
	for {
		n, err := rc.Read(readBuf)
		if n > 0 {
			chunk := readBuf[:n]
			mu.Lock()
			buf = append(buf, chunk...)
			full := len(buf) >= outputFlushBytes
			mu.Unlock()
			if reply != nil {
				reply.Write(chunk)
			}
			if full {
				flush()
			}
		}
		if err != nil {
			// Stop the ticker first so exactly one goroutine drains the tail.
			close(stopTick)
			ticker.Wait()
			flush()
			return
		}
	}
}

// Interrupt stops the running turn: SIGINT, a grace period, then SIGKILL. A
// non-empty turnID guards against a stale interrupt landing on a newer turn.
func (r *Runner) Interrupt(turnID string, grace time.Duration) error {
	r.mu.Lock()
	if r.active == nil {
		r.mu.Unlock()
		return ErrNoTurnRunning
	}
	if turnID != "" && turnID != r.turnID {
		running := r.turnID
		r.mu.Unlock()
		return fmt.Errorf("%w: %s is not running (%s is)", ErrNotFound, turnID, running)
	}
	r.mu.Unlock()

	if grace <= 0 {
		grace = r.grace()
	}
	return r.stop(grace)
}

// stop signals the active process group and escalates if it does not exit.
func (r *Runner) stop(grace time.Duration) error {
	r.mu.Lock()
	cmd := r.active
	if cmd == nil || cmd.Process == nil {
		r.mu.Unlock()
		return ErrNoTurnRunning
	}
	pid := cmd.Process.Pid
	r.interrupted = true
	r.mu.Unlock()

	// SIGINT first so the agent can flush the transcript --continue depends on.
	if err := signalProcessGroup(pid, sigInterrupt); err != nil {
		// Already gone.
		return nil
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !r.stillRunning(pid) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = signalProcessGroup(pid, sigKill)
	return nil
}

// stillRunning probes the process group with signal 0.
func (r *Runner) stillRunning(pid int) bool {
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if active == nil {
		return false
	}
	return signalProcessGroup(pid, 0) == nil
}

// RunningTurn reports the turn currently executing, if any.
func (r *Runner) RunningTurn() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnID
}

// Busy reports whether an agent process is live.
func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active != nil
}

// replyBuffer accumulates a bounded tail of output.
type replyBuffer struct {
	mu  sync.Mutex
	buf []byte
	// dropped records that the reply was clipped.
	dropped bool
}

func (b *replyBuffer) Write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > maxReplyBytes {
		// Keep the tail; the conclusion is at the end.
		b.buf = b.buf[len(b.buf)-maxReplyBytes:]
		b.dropped = true
	}
}

func (b *replyBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dropped {
		return "…(earlier output trimmed; the full turn is in the journal)\n" + string(b.buf)
	}
	return string(b.buf)
}
