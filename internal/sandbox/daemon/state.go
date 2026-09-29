package daemon

// State is a sandbox lifecycle state. Pausing and Resuming are transient, so
// exec is rejected mid-transition and a concurrent lifecycle call sees the in-flight op.
type State string

const (
	StatePending State = "pending"
	StateRunning State = "running"
	StatePausing State = "pausing"
	StatePaused  State = "paused"
	// StateStandby is frozen in RAM: the guest's memory, processes and open
	// sockets are all still there and the node still holds the RAM. Thawing is
	// milliseconds. StatePaused is the disk tier, where the memory is written
	// out and freed.
	StateStandby  State = "standby"
	StateResuming State = "resuming"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

// canTransition reports whether an event is legal from the given state.
func canTransition(from State, event string) bool {
	switch event {
	case "exec":
		// Still running-only: a frozen or paused sandbox is woken by
		// ensureRunning before the exec is attempted, not by this.
		return from == StateRunning
	case "standby":
		return from == StateRunning
	case "pause":
		// From standby too: the sandbox is thawed and then written to disk.
		return from == StateRunning || from == StateStandby
	case "resume":
		return from == StatePaused || from == StateStandby
	case "destroy":
		return from != StateStopped
	default:
		return false
	}
}
