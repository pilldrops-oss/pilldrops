package telegram

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RunOpts are the per-task decisions the bridge hands the runner.
type RunOpts struct {
	// FollowUp continues the agent's existing conversation instead of starting a new one.
	FollowUp bool
	// Autonomous grants the agent its own --dangerously-* flags; without them it keeps its approval prompts.
	Autonomous bool
}

// Runner executes a task on a workstation, resuming it first if paused.
type Runner interface {
	Run(ctx context.Context, sandboxID, task string, opts RunOpts) (string, error)
}

// riskyPatterns match tasks whose stated intent is irreversible. This is
// intent matching on the user's words, not a sandbox, and it is biased toward
// false positives since an extra tap is cheap.
var riskyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+-[a-z]*[rf]`),
	regexp.MustCompile(`(?i)\bforce[- ]?push|push\s+--force|\bgit\s+push\s+-f\b`),
	regexp.MustCompile(`(?i)\bdrop\s+(table|database|schema)\b`),
	regexp.MustCompile(`(?i)\btruncate\s+table\b`),
	regexp.MustCompile(`(?i)\bdeploy(ing|ment)?\b.{0,30}\b(prod|production|live)\b`),
	regexp.MustCompile(`(?i)\b(prod|production)\b.{0,30}\bdeploy`),
	regexp.MustCompile(`(?i)\bmigrat(e|ion)\b.{0,30}\b(prod|production)\b`),
	regexp.MustCompile(`(?i)\bgit\s+reset\s+--hard\b`),
	regexp.MustCompile(`(?i)\bdelete\b.{0,30}\b(branch|repo|repository|bucket|volume|database)\b`),
	regexp.MustCompile(`(?i)\brevoke|rotate\b.{0,20}\b(key|token|credential|secret)s?\b`),
	regexp.MustCompile(`(?i)\bpublish\b.{0,20}\b(npm|package|release)\b`),
	regexp.MustCompile(`(?i)\bsend\b.{0,20}\bemail|\bemail\b.{0,20}\b(customer|everyone|list)\b`),
}

// IsRisky reports whether a task's stated intent warrants a confirmation tap.
func IsRisky(task string) bool {
	t := strings.TrimSpace(task)
	for _, re := range riskyPatterns {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// pendingTTL bounds how long a confirmation stays answerable.
const pendingTTL = 5 * time.Minute

// pending is a task awaiting a confirmation tap.
type pending struct {
	chat     string
	task     string
	followUp bool
	at       time.Time
}

// pendingSet holds unconfirmed tasks keyed by the id carried in the button's callback data.
type pendingSet struct {
	mu sync.Mutex
	m  map[string]pending
	n  uint64
}

func newPendingSet() *pendingSet { return &pendingSet{m: map[string]pending{}} }

func (p *pendingSet) add(chat, task string, followUp bool, now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweep(now)
	p.n++
	id := strings.ToLower(strings.TrimSpace(strconv.FormatUint(p.n, 10)))
	p.m[id] = pending{chat: chat, task: task, followUp: followUp, at: now}
	return id
}

// take removes and returns a pending task. The chat must match so a forged callback cannot run someone else's task.
func (p *pendingSet) take(id, chat string, now time.Time) (pending, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweep(now)
	got, ok := p.m[id]
	if !ok || got.chat != chat {
		return pending{}, false
	}
	delete(p.m, id)
	return got, true
}

// sweep drops expired entries; called under the lock on every access.
func (p *pendingSet) sweep(now time.Time) {
	for id, v := range p.m {
		if now.Sub(v.at) > pendingTTL {
			delete(p.m, id)
		}
	}
}
