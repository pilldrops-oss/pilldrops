package agentd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Approval is one parked tool call.
type Approval struct {
	ID        string `json:"id"`
	TurnID    string `json:"turn_id"`
	Tool      string `json:"tool"`
	ToolUseID string `json:"tool_use_id,omitempty"`

	// Signature is what a "remember this" decision applies to, in the words a UI should show.
	Signature string `json:"signature"`

	// ArgsDigest identifies the exact arguments without storing them.
	ArgsDigest string `json:"args_digest"`
	// ArgsPreview is redacted and truncated. Raw arguments are never stored, since
	// file contents routinely contain credentials.
	ArgsPreview string `json:"args_preview"`

	RequestedAt time.Time `json:"requested_at"`

	// Decision is empty while parked.
	Decision  Decision      `json:"decision,omitempty"`
	DecidedBy string        `json:"decided_by,omitempty"`
	DecidedAt time.Time     `json:"decided_at,omitempty"`
	Remember  RememberScope `json:"remember,omitempty"`
	// Reason explains an automatic outcome (a policy allow, or a timeout).
	Reason string `json:"reason,omitempty"`
}

// RememberScope is how long a decision applies.
type RememberScope string

const (
	// RememberOnce applies to this call only.
	RememberOnce RememberScope = "once"
	// RememberSession applies until this agentd restarts.
	RememberSession RememberScope = "session"
	// RememberSandbox applies for as long as the sandbox exists; persisted.
	RememberSandbox RememberScope = "sandbox"
)

// ValidRememberScope reports whether s is an offered scope. There is no org
// scope on purpose: a blanket allow has no undo.
func ValidRememberScope(s RememberScope) bool {
	switch s {
	case RememberOnce, RememberSession, RememberSandbox:
		return true
	}
	return false
}

// ErrNoSuchApproval means the approval is not parked: decided, timed out, or never existed.
var ErrNoSuchApproval = errors.New("no such pending approval")

// approvalTimeout is how long a parked call waits before being denied. A gate
// that allows on timeout is a time-delayed autonomous agent.
const approvalTimeout = 15 * time.Minute

// maxPendingApprovals bounds the parked queue, since a prompt injection could
// flood it. Hitting it denies, same direction as a timeout.
const maxPendingApprovals = 32

// maxResolvedKept bounds the ring of finished approvals held for the node to collect.
const maxResolvedKept = 200

// maxPreviewBytes bounds the redacted preview; a Write's input is a whole file.
const maxPreviewBytes = 2 << 10

// Gate evaluates policy and parks what it cannot decide alone.
type Gate struct {
	Journal *Journal
	// Policy is consulted first. A rule match is an allow with a recorded reason.
	Policy Policy
	// Redactor strips known secrets from previews. Nil means pattern-based redaction only.
	Redactor *Redactor
	// Timeout overrides approvalTimeout; tests set it.
	Timeout time.Duration
	// OnPark reports that a turn is (or is no longer) waiting on a human. Nil
	// means nobody is listening.
	OnPark func(turnID string, waiting bool)
	// Remembered persists sandbox-scoped decisions. Nil disables that scope,
	// reported to the caller rather than downgraded to session.
	Remembered *RememberStore

	now func() time.Time

	mu      sync.Mutex
	seq     int
	pending map[string]*parked
	// session holds signatures remembered for this agentd's lifetime.
	session map[string]bool
	// consulted counts requests that reached this gate, compared against tool
	// calls seen in the stream to prove the prompt tool was not ignored.
	consulted int
	// resolved is a bounded ring of finished approvals for the node to collect,
	// since the journal dies with the sandbox.
	resolved []Approval
}

type parked struct {
	a    Approval
	done chan Approval
}

// NewGate builds a gate. An empty Policy asks about everything, so it defaults to DefaultPolicy.
func NewGate(j *Journal, p Policy) *Gate {
	if len(p.Rules) == 0 {
		p = DefaultPolicy
	}
	return &Gate{
		Journal: j, Policy: p, now: time.Now,
		pending: map[string]*parked{}, session: map[string]bool{},
	}
}

func (g *Gate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *Gate) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return approvalTimeout
}

// Consulted reports how many decisions this gate has been asked for.
func (g *Gate) Consulted() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.consulted
}

// park notifies the OnPark listener, if there is one.
func (g *Gate) park(turnID string, waiting bool) {
	if g.OnPark != nil {
		g.OnPark(turnID, waiting)
	}
}

// Request decides one tool call, blocking until it can answer. Never returns
// DecisionAsk; that is an internal state, not an outcome.
func (g *Gate) Request(ctx context.Context, turnID string, c ToolCall) (Decision, string) {
	sig := Signature(c)

	g.mu.Lock()
	g.consulted++
	// Remembered allows come before policy so the reason credits the user's grant, not a rule.
	remembered := g.session[sig] || (g.Remembered != nil && g.Remembered.Has(sig))
	g.mu.Unlock()

	if remembered {
		g.journal(EventApprovalGranted, turnID, map[string]any{
			"tool": c.Tool, "signature": sig, "tool_use_id": c.ID,
			"reason": "previously allowed for this " + g.rememberedScopeLabel(sig),
			"auto":   true,
		})
		g.recordAuto(turnID, c, sig, "previously allowed for this "+g.rememberedScopeLabel(sig))
		return DecisionAllow, ""
	}

	if d, rule := g.Policy.Evaluate(c); d == DecisionAllow {
		// Journalled so "it ran without asking" has a reason on record.
		g.journal(EventApprovalGranted, turnID, map[string]any{
			"tool": c.Tool, "signature": sig, "tool_use_id": c.ID,
			"reason": "allowed by policy: " + rule.Why, "auto": true,
		})
		g.recordAuto(turnID, c, sig, "allowed by policy: "+rule.Why)
		return DecisionAllow, ""
	}

	a := Approval{
		TurnID: turnID, Tool: c.Tool, ToolUseID: c.ID, Signature: sig,
		ArgsDigest: ArgsDigest(c.Input), RequestedAt: g.clock(),
	}
	a.ArgsPreview = g.preview(c.Input)

	g.mu.Lock()
	if len(g.pending) >= maxPendingApprovals {
		g.mu.Unlock()
		// Journalled: a flood is a signal about the agent.
		g.journal(EventApprovalDenied, turnID, map[string]any{
			"tool": c.Tool, "signature": sig, "tool_use_id": c.ID,
			"reason": "refused without review: too many actions are already waiting",
			"auto":   true, "pending": maxPendingApprovals,
		})
		return DecisionAsk, "too many actions are already waiting for approval, so this " +
			"one was refused without review. Ask the user to work through the queue."
	}
	g.seq++
	a.ID = newApprovalID(a.RequestedAt)
	p := &parked{a: a, done: make(chan Approval, 1)}
	g.pending[a.ID] = p
	g.mu.Unlock()

	g.journal(EventApprovalRequested, turnID, map[string]any{
		"approval_id": a.ID, "tool": a.Tool, "tool_use_id": a.ToolUseID,
		"signature": a.Signature, "args_digest": a.ArgsDigest,
		"args_preview": a.ArgsPreview,
	})

	// Cleared on every exit so a resumed turn does not stay marked awaiting.
	g.park(turnID, true)
	defer g.park(turnID, false)

	timer := time.NewTimer(g.timeout())
	defer timer.Stop()

	select {
	case decided := <-p.done:
		if decided.Decision == DecisionAllow {
			return DecisionAllow, ""
		}
		msg := decided.Reason
		if msg == "" {
			msg = "denied by a human reviewer"
		}
		return DecisionAsk, msg

	case <-timer.C:
		// finish can lose a race with a concurrent decision; log it but still report the timeout.
		if _, err := g.finish(a.ID, DecisionAsk, "", RememberOnce,
			fmt.Sprintf("no answer within %s, so it was denied", g.timeout())); err != nil {
			log.Printf("[agentd] gate: finish(%s) on timeout: %v", a.ID, err)
		}
		g.journal(EventApprovalTimeout, turnID, map[string]any{
			"approval_id": a.ID, "tool": a.Tool, "waited": g.timeout().String(),
		})
		// Denied, not allowed. See approvalTimeout.
		return DecisionAsk, "nobody approved this within " + g.timeout().String() +
			", so it was denied. Ask the user to approve it and try again."

	case <-ctx.Done():
		// Nobody said yes, so deny, same as a timeout.
		if _, err := g.finish(a.ID, DecisionAsk, "", RememberOnce, "the turn ended before this was reviewed"); err != nil {
			log.Printf("[agentd] gate: finish(%s) on interrupt: %v", a.ID, err)
		}
		return DecisionAsk, "the turn ended before this action was reviewed, so it did not run"
	}
}

// Pending lists parked approvals, oldest first, the order in which they time out.
func (g *Gate) Pending() []Approval {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Approval, 0, len(g.pending))
	for _, p := range g.pending {
		out = append(out, p.a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.Before(out[j].RequestedAt) })
	return out
}

// Decide answers a parked approval. Who may call it is enforced at the HTTP
// layer; the gate cannot tell a robot from a person.
func (g *Gate) Decide(id string, d Decision, remember RememberScope, actor string) error {
	if d != DecisionAllow && d != DecisionAsk {
		return fmt.Errorf("decision must be %q or %q", DecisionAllow, DecisionAsk)
	}
	if remember == "" {
		remember = RememberOnce
	}
	if !ValidRememberScope(remember) {
		return fmt.Errorf("unknown remember scope %q", remember)
	}
	// A remembered deny would silently refuse later calls with no way to undo.
	if d != DecisionAllow {
		remember = RememberOnce
	}

	a, err := g.finish(id, d, actor, remember, "")
	if err != nil {
		return err
	}

	if d == DecisionAllow && remember != RememberOnce {
		g.mu.Lock()
		g.session[a.Signature] = true
		g.mu.Unlock()
		if remember == RememberSandbox && g.Remembered != nil {
			// Report a persist failure rather than silently downgrade to session scope.
			if perr := g.Remembered.Add(a.Signature); perr != nil {
				g.journal(EventApprovalGranted, a.TurnID, map[string]any{
					"approval_id": a.ID, "signature": a.Signature,
					"warning": "allowed, but could not be remembered beyond this session: " + perr.Error(),
				})
			}
		}
	}

	ev := EventApprovalGranted
	if d != DecisionAllow {
		ev = EventApprovalDenied
	}
	g.journal(ev, a.TurnID, map[string]any{
		"approval_id": a.ID, "tool": a.Tool, "tool_use_id": a.ToolUseID,
		"signature": a.Signature, "decided_by": actor, "remember": string(remember),
	})
	return nil
}

// finish removes a parked approval and wakes its waiter.
func (g *Gate) finish(id string, d Decision, actor string, remember RememberScope, reason string) (Approval, error) {
	g.mu.Lock()
	p, ok := g.pending[id]
	if !ok {
		g.mu.Unlock()
		return Approval{}, ErrNoSuchApproval
	}
	delete(g.pending, id)
	p.a.Decision, p.a.DecidedBy, p.a.DecidedAt = d, actor, g.clock()
	p.a.Remember, p.a.Reason = remember, reason
	a := p.a
	g.resolved = append(g.resolved, a)
	if len(g.resolved) > maxResolvedKept {
		// Drop the oldest; the node is expected to have persisted anything older.
		g.resolved = g.resolved[len(g.resolved)-maxResolvedKept:]
	}
	g.mu.Unlock()

	p.done <- a
	return a, nil
}

func (g *Gate) rememberedScopeLabel(sig string) string {
	if g.Remembered != nil && g.Remembered.Has(sig) {
		return "sandbox"
	}
	return "session"
}

func (g *Gate) journal(event, turnID string, fields map[string]any) {
	if g.Journal == nil {
		return
	}
	_, _ = g.Journal.Append(event, turnID, fields)
}

// preview renders arguments for a human, redacted and truncated.
func (g *Gate) preview(input map[string]any) string {
	b, err := json.Marshal(input)
	if err != nil {
		return "(arguments could not be rendered)"
	}
	s := string(b)
	if g.Redactor != nil {
		s = g.Redactor.Redact(s)
	}
	if len(s) > maxPreviewBytes {
		// State the truncation so a reviewer knows they see only part of the payload.
		s = s[:maxPreviewBytes] + fmt.Sprintf("… (%d bytes truncated)", len(s)-maxPreviewBytes)
	}
	return s
}

// ArgsDigest is a stable sha256 over tool arguments; encoding/json sorts map
// keys, so key order cannot change it.
func ArgsDigest(input map[string]any) string {
	b, err := json.Marshal(input)
	if err != nil {
		// Digest something rather than empty, which would collide with "no arguments".
		b = []byte(fmt.Sprintf("%#v", input))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Signature is the category a "remember this" decision applies to: program plus
// subcommand for a shell call, tool plus directory for a file call. So "npm
// install left-pad" also allows "npm install react" but not "npm publish".
func Signature(c ToolCall) string {
	if cmd, ok := c.command(); ok {
		argv, _, parsed := lexCommand(cmd)
		if parsed && len(argv) > 0 {
			prog := path.Base(argv[0])
			// A second word counts only when it looks like a subcommand, so
			// "cat /etc/passwd" is not its own category.
			if len(argv) > 1 && isSubcommandWord(argv[1]) {
				return c.Tool + ": " + prog + " " + argv[1] + " *"
			}
			return c.Tool + ": " + prog + " *"
		}
		// Unparseable: a signature that matches nothing else, so a remembered allow cannot be reused.
		return c.Tool + ": " + ArgsDigest(c.Input)[:12]
	}
	if p := c.filePath(); p != "" {
		return c.Tool + ": " + path.Dir(path.Clean(p)) + "/*"
	}
	return c.Tool
}

// isSubcommandWord reports whether a word reads as a subcommand rather than a path or flag.
func isSubcommandWord(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	if strings.ContainsAny(s, "/.=") {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// forgetSession drops a session-scoped grant so a revoked grant stops working immediately.
func (g *Gate) forgetSession(sig string) {
	g.mu.Lock()
	delete(g.session, sig)
	g.mu.Unlock()
}

// newApprovalID returns a globally unique id. A per-process counter would not
// survive a restart, so a stale dashboard tab could approve a later action
// that reused the id.
func newApprovalID(at time.Time) string {
	var r [6]byte
	_, _ = rand.Read(r[:])
	if at.IsZero() {
		at = time.Now()
	}
	return fmt.Sprintf("apr_%012x%s", at.UTC().UnixMilli(), hex.EncodeToString(r[:]))
}

// Resolved returns finished approvals, oldest first, for the node to persist.
// Policy allows and timeouts are included so the unasked cases leave a record.
func (g *Gate) Resolved() []Approval {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Approval, len(g.resolved))
	copy(out, g.resolved)
	return out
}

// recordAuto adds an automatically-decided approval to the resolved ring; it
// never parked, so it never passed through finish.
func (g *Gate) recordAuto(turnID string, c ToolCall, sig, reason string) {
	a := Approval{
		ID: newApprovalID(g.clock()), TurnID: turnID, Tool: c.Tool, ToolUseID: c.ID,
		Signature: sig, ArgsDigest: ArgsDigest(c.Input),
		RequestedAt: g.clock(), Decision: DecisionAllow, DecidedAt: g.clock(),
		DecidedBy: "policy", Reason: reason,
	}
	g.mu.Lock()
	g.resolved = append(g.resolved, a)
	if len(g.resolved) > maxResolvedKept {
		g.resolved = g.resolved[len(g.resolved)-maxResolvedKept:]
	}
	g.mu.Unlock()
}

// SessionGrants lists signatures remembered for this agentd's lifetime only,
// separate from the persisted ones. A grant nobody can enumerate is a grant
// nobody can revoke.
func (g *Gate) SessionGrants() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Sorted(maps.Keys(g.session))
}
