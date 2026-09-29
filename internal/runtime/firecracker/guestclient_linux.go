package firecracker

import (
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	"github.com/pilldrops-oss/pilldrops/internal/vm"
)

// guestClient returns the VM's envd client. Only the Linux vm.Sandbox carries
// one; the stub used off-Linux has no guest to talk to.
func guestClient(sb *vm.Sandbox) *envd.Client { return sb.Envd }
