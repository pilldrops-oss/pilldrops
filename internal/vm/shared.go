package vm

import (
	"errors"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// busyErr is a refusal that is about this node, not the request: each one is
// also runtime.ErrNodeBusy, so the control plane tries another node.
type busyErr string

func (e busyErr) Error() string      { return string(e) }
func (busyErr) Is(target error) bool { return target == runtime.ErrNodeBusy }

// Everything in this file is identical on every platform, so it lives outside
// the build tags. Each of these used to be written twice -- once in the Linux
// engine and once in stub_other.go, under a comment asking the next person to
// keep the two in step by hand.

var (
	ErrSandboxNotFound       = errors.New("sandbox not found")
	ErrTooManyRequests error = busyErr("server busy")
	ErrServerFull      error = busyErr("max sandboxes reached")
	ErrDiskPressure    error = busyErr("server disk pressure, try again later")
	// ErrForkNeedsNetNS: on the shared bridge the parent's tap name and address
	// are live, so a child carrying the same baked identity has nowhere to
	// attach. Declared here so callers can compare against it on any platform.
	ErrForkNeedsNetNS = errors.New("fork requires per-sandbox network namespaces (FC_NETNS)")
)

// DefaultOverlayMB is the writable-disk size used when a caller declares none;
// an unlimited plan still needs a number for a file created before the guest boots.
const DefaultOverlayMB = 20 * 1024

// SandboxInfo is what the node reports about a live sandbox.
type SandboxInfo struct {
	ID        string    `json:"id"`
	Template  string    `json:"template"`
	UserID    string    `json:"user_id"`
	BootType  string    `json:"boot_type"`
	BootMs    int64     `json:"boot_ms"`
	VMIP      string    `json:"vm_ip"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
