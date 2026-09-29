package runtime

// IsolationLevel is the tenant boundary a runtime provides, as a caller asks
// for it. Lives here because both the node and the control plane need it and
// neither may import the other.
type IsolationLevel string

const (
	// IsolationStrong means the guest has its own kernel.
	IsolationStrong IsolationLevel = "strong"
	// IsolationShared means a namespace/cgroup/seccomp boundary on the host kernel.
	IsolationShared IsolationLevel = "shared"
	// IsolationTrusted means no tenant boundary is claimed.
	IsolationTrusted IsolationLevel = "trusted"
)

// ownKernel is keyed by runtime name because the name is all the control plane
// gets from a heartbeat.
var ownKernel = map[string]bool{
	"firecracker": true,
}

// LevelFor maps a runtime name and its Caps.Isolation bit to a level. The bit
// is checked first so a runtime cannot name its way out of trusted, and unknown
// names default to shared, never strong.
func LevelFor(name string, isolating bool) IsolationLevel {
	if !isolating {
		return IsolationTrusted
	}
	if ownKernel[name] {
		return IsolationStrong
	}
	return IsolationShared
}

// IsolationLevels lists the levels strongest first; callers rely on the order.
func IsolationLevels() []IsolationLevel {
	return []IsolationLevel{IsolationStrong, IsolationShared, IsolationTrusted}
}
