//go:build !linux

package firecracker

import (
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	"github.com/pilldrops-oss/pilldrops/internal/vm"
)

// guestClient is nil off-Linux: the vm stub boots nothing, so the adapter
// takes its buffered, backend-only paths (what the unit tests exercise).
func guestClient(*vm.Sandbox) *envd.Client { return nil }
