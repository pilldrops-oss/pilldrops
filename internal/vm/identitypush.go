//go:build linux

package vm

// Host end of the sandbox-identity push. The guest's own pd_sb_ token goes
// over the same per-VM vsock UDS as SETHASH/SETNET; envd writes it to
// /run/pilldrops (cmd/envd/identity.go). Pushed after cold boot, golden
// restore and fork, never after a resume: a resumed guest already holds it.

import (
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// credSetIdentityCommand mirrors cmd/envd/identity.go's credSetIdentity.
const credSetIdentityCommand = "SETIDENTITY"

// ErrIdentityPush wraps every identity-delivery failure so a create error names it.
var ErrIdentityPush = errors.New("could not deliver the sandbox identity to the guest")

// identityCommand renders the SETIDENTITY line; the format is fixed by cmd/envd/identity.go.
func identityCommand(id runtime.Identity) string {
	domain := id.Domain
	if domain == "" {
		domain = "-"
	}
	return fmt.Sprintf("%s %s %s %s %s", credSetIdentityCommand, id.Token, id.APIURL, id.SandboxID, domain)
}

// DeliverIdentity pushes a booted sandbox's own credential into its guest.
// With PILLDROPS_LOCAL_API_PORT set (a devserver on this host, which the
// guest is otherwise firewalled from) the API URL becomes the slot's host
// veth address on that port, the one hole buildNetNSRuleset opens.
func (m *Manager) DeliverIdentity(sandboxID string, id runtime.Identity) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s: %w", ErrIdentityPush, sandboxID, ErrSandboxNotFound)
	}
	if port := localAPIPort(); port != 0 {
		slot, _, ok := parseNetNSName(sb.NetNS)
		if !ok {
			return fmt.Errorf("%w: PILLDROPS_LOCAL_API_PORT is set but %s has no netns slot to reach the host through", ErrIdentityPush, sandboxID)
		}
		id.APIURL = fmt.Sprintf("http://%s:%d", slotIP(slot, 1), port)
	}
	if err := pushToGuest(sb.Machine.ChrootRoot(), sandboxID, identityCommand(id), credPushDeadline); err != nil {
		return fmt.Errorf("%w: %w", ErrIdentityPush, err)
	}
	// A created VM runs under its own id, which is what its hostname was set to;
	// the guest should answer to the sandbox id the user sees. Cosmetic, so not fatal.
	if id.SandboxID != "" && id.SandboxID != sandboxID {
		cmd := fmt.Sprintf("%s %s", credSetHostCommand, id.SandboxID)
		if err := pushToGuest(sb.Machine.ChrootRoot(), sandboxID, cmd, hostnamePushDeadline); err != nil {
			m.log.Warn("guest_hostname_not_set", zap.String("sandbox_id", id.SandboxID), zap.Error(err))
		}
	}
	return nil
}
