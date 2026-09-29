package telegram

import (
	"errors"
	"sync"
)

// SandboxGate serializes agent turns per workstation: two turns racing on one
// machine corrupt the agent's on-disk transcript silently. The gate is
// process-wide because runners are constructed per request.
type SandboxGate struct {
	// disabled makes every acquire succeed (see NoGate).
	disabled bool

	mu   sync.Mutex
	held map[string]struct{}
}

// ErrBusy reports that a turn is already running on this workstation. The text is shown verbatim to the user.
var ErrBusy = errors.New("the agent is still working on your previous message — send this again when it replies")

// processGate is the gate every caller shares unless it says otherwise.
var processGate = &SandboxGate{held: map[string]struct{}{}}

// NoGate always acquires. Use it as DaemonRunner.Gate when the caller already holds the turn slot.
var NoGate = &SandboxGate{disabled: true}

// NewSandboxGate returns an independent gate for tests.
func NewSandboxGate() *SandboxGate { return &SandboxGate{held: map[string]struct{}{}} }

// acquire takes the turn slot for sandboxID, reporting false when a turn is
// already running there. It never blocks, since a caller waiting behind a
// long agent run is a phone that looks frozen.
func (g *SandboxGate) acquire(sandboxID string) bool {
	if g == nil {
		return processGate.acquire(sandboxID)
	}
	if g.disabled {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held == nil {
		g.held = map[string]struct{}{}
	}
	if _, busy := g.held[sandboxID]; busy {
		return false
	}
	g.held[sandboxID] = struct{}{}
	return true
}

// release frees the turn slot. Deleting the key keeps the map proportional to running turns.
func (g *SandboxGate) release(sandboxID string) {
	if g == nil {
		processGate.release(sandboxID)
		return
	}
	if g.disabled {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.held, sandboxID)
}

// AcquireTurn takes the process-wide turn slot for a workstation and returns
// the function that frees it. For callers that must learn about contention
// before committing anything.
func AcquireTurn(sandboxID string) (release func(), ok bool) {
	if !processGate.acquire(sandboxID) {
		return func() {}, false
	}
	return func() { processGate.release(sandboxID) }, true
}
