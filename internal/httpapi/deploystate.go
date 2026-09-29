package httpapi

import (
	"sync"
	"time"
)

// Progress of the files/setup/start work after a create, for clients that opt
// out of the synchronous response and poll instead.

// DeployState is the lifecycle of one sandbox's post-create work.
type DeployState string

const (
	DeployRunning DeployState = "running"
	DeployDone    DeployState = "done"
	// DeployFailed means a setup command exited non-zero or the start command could not launch.
	DeployFailed DeployState = "failed"
)

// DeployStep is one command and how it went.
type DeployStep struct {
	Cmd string `json:"cmd"`
	// Exit is -1 when the command could not be run at all.
	Exit int    `json:"exit"`
	Err  string `json:"err,omitempty"`
	MS   int64  `json:"ms"`
}

// DeployProgress is what a client polls while a deploy runs.
type DeployProgress struct {
	State DeployState  `json:"state"`
	Steps []DeployStep `json:"steps"`
	// Log carries the same lines the synchronous create response returns.
	Log []string `json:"log"`
}

// deployTracker holds progress for in-flight and recent deploys. In memory on
// purpose: after a restart a poller gets "unknown", not a fabricated history.
type deployTracker struct {
	mu   sync.RWMutex
	byID map[string]*trackedDeploy
}

type trackedDeploy struct {
	progress DeployProgress
	at       time.Time
}

func newDeployTracker() *deployTracker {
	return &deployTracker{byID: map[string]*trackedDeploy{}}
}

func (t *deployTracker) start(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID[id] = &trackedDeploy{progress: DeployProgress{State: DeployRunning}, at: time.Now()}
	t.evictLocked()
}

func (t *deployTracker) step(id string, s DeployStep, line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.byID[id]
	if !ok {
		return
	}
	d.progress.Steps = append(d.progress.Steps, s)
	d.progress.Log = append(d.progress.Log, line)
}

func (t *deployTracker) finish(id string, state DeployState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d, ok := t.byID[id]; ok {
		d.progress.State = state
		d.at = time.Now()
	}
}

// get returns progress and whether anything is known; unknown is never reported as done.
func (t *deployTracker) get(id string) (DeployProgress, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	d, ok := t.byID[id]
	if !ok {
		return DeployProgress{}, false
	}
	// Copy: the deploy goroutine appends to these slices concurrently.
	out := DeployProgress{State: d.progress.State}
	out.Steps = append([]DeployStep(nil), d.progress.Steps...)
	out.Log = append([]string(nil), d.progress.Log...)
	return out, true
}

// evictLocked drops finished deploys older than the retention window. Caller holds the write lock.
func (t *deployTracker) evictLocked() {
	const keep = 30 * time.Minute
	if len(t.byID) < 256 {
		return
	}
	cut := time.Now().Add(-keep)
	for id, d := range t.byID {
		if d.progress.State != DeployRunning && d.at.Before(cut) {
			delete(t.byID, id)
		}
	}
}
