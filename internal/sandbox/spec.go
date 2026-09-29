// Package sandbox holds the create request and the pure policy applied to it:
// validation, resource resolution, plan clamping and lifetime. It is stateless
// and linked by both tiers; sandbox/daemon is node-only.
package sandbox

import (
	"fmt"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// Spec is a create request, shared by both tiers.
type Spec struct {
	Template string `json:"template"`
	// Snapshot names a saved snapshot to wake from instead of booting a
	// template. Mutually exclusive with Template: the control plane resolves it
	// to the snapshot's own template id and fixes VCPU and MemoryMB from it,
	// since a restore cannot be resized.
	Snapshot string `json:"snapshot"`
	// ID is set only by the control plane moving a sandbox between nodes; the
	// node honours it from an operator caller and refuses it from anyone else.
	ID string `json:"id,omitempty"`
	// Name is an optional human label, validated like a rename.
	Name string `json:"name"`
	// Slug is an optional DNS label the sandbox is hosted under (<slug>.<domain>);
	// see ValidSlug. Unique across the platform.
	Slug    string `json:"slug"`
	Timeout int    `json:"timeout"`
	// IdleTimeout in seconds: 0 = platform default, negative = never. It is the
	// standby timer — the sandbox is frozen in RAM, not written to disk.
	IdleTimeout int `json:"idle_timeout"`
	// HibernateTimeout in seconds: 0 = platform default (4 h), negative = never.
	// It is the disk timer, below standby.
	HibernateTimeout int `json:"hibernate_timeout"`
	// Resources; 0 falls back to the template's recommendation. Capped server-side.
	VCPU      int      `json:"vcpu"`
	MemoryMB  int      `json:"memory_mb"`
	StorageMB int      `json:"storage_mb"`
	Files     []File   `json:"files"`
	Setup     []string `json:"setup"`
	// AsyncSetup returns the id immediately and runs files, setup and start in the background.
	AsyncSetup bool `json:"async_setup"`
	// Repo is a git URL cloned before the terminal opens; RepoRef selects a branch or tag.
	Repo    string `json:"repo"`
	RepoRef string `json:"repo_ref"`
	Start   string `json:"start"`
	Port    int    `json:"port"`
	// Image, if set, replaces the base sandbox image with its own entrypoint, env and ports.
	Image string   `json:"image"`
	Ports []int    `json:"ports"`
	Env   []string `json:"env"` // "NAME=VALUE"
	// Client tags who is creating the sandbox, for attribution.
	Client string `json:"client"`
	// AgentMode is the autonomy posture an agent workstation starts in.
	AgentMode string `json:"agent_mode"`
	// Zone and Runtime are placement hints the control plane reads; Isolation is an alias for Runtime.
	Zone      string `json:"zone"`
	Runtime   string `json:"runtime"`
	Isolation string `json:"isolation"`
	// Public opts the preview out of the token gate.
	Public bool `json:"public"`
	// Visibility is "org" (the default: every key in the org sees it) or
	// "private" (only the creating key and operators). Empty means "org".
	Visibility string `json:"visibility,omitempty"`
	// Network is the egress policy requested. nil means nothing requested, not
	// allow-all; a request can only narrow the platform's policy.
	Network *egress.Rules `json:"network,omitempty"`
	// Networks are the opt-in network tags this sandbox joins. Empty is the
	// default and means it reaches no other sandbox and none reaches it; two
	// sandboxes in the same org on the same node reach each other only when
	// they share a tag. See ValidNetworks.
	Networks []string `json:"networks,omitempty"`
}

// File is one file written into the sandbox at create time.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Limits is the subset of a billing plan that create policy consults; plain
// numbers so this package does not depend on billing.
type Limits struct {
	ID            string
	MaxVCPU       int
	MaxMemMB      int
	MaxStorageMB  int
	MaxConcurrent int
	// MaxWorkstations caps persistent sandboxes separately from MaxConcurrent,
	// since their disk is billed while paused and held indefinitely.
	MaxWorkstations int
	SessionTTLHrs   int
	// Persistent reports whether the plan sells a no-expiry workstation.
	Persistent bool
}

// TemplateDefaults is what the catalogue recommends for a template; zero values
// mean no recommendation, as for an image-backed service not in the catalogue.
type TemplateDefaults struct {
	ID        string
	CPU       int
	MemoryMB  int
	StorageMB int
	Port      int
	// Network is the template's default egress policy, the base a request
	// narrows from absent a per-org override. nil imposes nothing.
	Network *egress.Rules
}

// Resolved is what will actually be provisioned after policy is applied.
type Resolved struct {
	// TemplateID is the canonical id, not the caller's alias; the VM layer and
	// pools key on it.
	TemplateID string
	Name       string
	VCPU       int
	MemMiB     int
	StorageMB  int
	// TTL is the hard lifetime; negative means persistent.
	TTL time.Duration
	// IdleTimeout is the standby timer: 0 = platform default, negative = never.
	IdleTimeout time.Duration
	// HibernateTimeout is the disk-pause timer; negative = never.
	HibernateTimeout time.Duration
	// Port is the app port that will be published.
	Port int
	// Env is the request env plus anything policy injected.
	Env []string
	// Network is the egress policy in force; nil means unrestricted.
	Network *egress.Rules
	// NetworkAdvisories are legal but probably unintended aspects of the policy; never an error.
	NetworkAdvisories []string
	// Networks are the validated network tags the sandbox joins; empty = none.
	Networks []string
}

// InvalidError is a caller mistake; the HTTP layer maps it to 400.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

// LimitError is a plan or capacity refusal; the HTTP layer maps it to 429 and honours RetryAfter.
type LimitError struct {
	Reason     string
	RetryAfter int // seconds; 0 = omit the header
}

func (e *LimitError) Error() string { return e.Reason }

func invalid(format string, a ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, a...)}
}

func limited(retryAfter int, format string, a ...any) error {
	return &LimitError{Reason: fmt.Sprintf(format, a...), RetryAfter: retryAfter}
}
