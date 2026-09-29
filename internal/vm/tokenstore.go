//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"strings"
)

// Per-sandbox state records (token, endpoint, tap) that orphan recovery and the
// startup sweeps read after a restart. Plaintext is acceptable: the threat is a
// neighbour VM, not a process that already has node root.

// tokenDir is a sibling of the overlay dir so records share its lifecycle.
func (m *Manager) tokenDir() string {
	return filepath.Join(filepath.Dir(m.config.OverlayDir), "envd-tokens")
}

func (m *Manager) tokenPath(id string) string {
	return filepath.Join(m.tokenDir(), id+".tok")
}

// saveToken is a no-op for the empty (auth-off) token.
func (m *Manager) saveToken(id, token string) {
	if token == "" {
		return
	}
	dir := m.tokenDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(m.tokenPath(id), []byte(token), 0o600)
}

// loadToken returns the persisted token, or "" when there is none.
func (m *Manager) loadToken(id string) string {
	b, err := os.ReadFile(m.tokenPath(id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) removeToken(id string) { _ = os.Remove(m.tokenPath(id)) }

// The envd address cannot be recomputed after a restart: guest IPs come from a
// counter, not from the sandbox id.
func (m *Manager) endpointPath(id string) string {
	return filepath.Join(m.tokenDir(), id+".addr")
}

// saveEndpoint records where a sandbox's envd answers ("<vm-ip>:6001").
func (m *Manager) saveEndpoint(id, addr string) {
	if addr == "" {
		return
	}
	if err := os.MkdirAll(m.tokenDir(), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(m.endpointPath(id), []byte(addr), 0o600)
}

// loadEndpoint returns the persisted envd address, or "" when there is none.
// Callers must treat "" as "cannot reconnect", never as "dial the default".
func (m *Manager) loadEndpoint(id string) string {
	b, err := os.ReadFile(m.endpointPath(id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) removeEndpoint(id string) { _ = os.Remove(m.endpointPath(id)) }

// Tap records scope the startup sweep to this daemon's own taps, so co-hosted
// daemons never delete each other's. Only bridge-mode fctapN taps are recorded;
// a netns tap is reclaimed with its namespace.
func (m *Manager) tapPath(id string) string {
	return filepath.Join(m.tokenDir(), id+".tap")
}

func (m *Manager) saveTap(id, tapName string) {
	if tapName == "" || !strings.HasPrefix(tapName, "fctap") {
		return
	}
	if err := os.MkdirAll(m.tokenDir(), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(m.tapPath(id), []byte(tapName), 0o600)
}

func (m *Manager) loadTap(id string) string {
	b, err := os.ReadFile(m.tapPath(id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) removeTap(id string) { _ = os.Remove(m.tapPath(id)) }

// tapRecords returns every persisted sandbox-to-tap pair in this daemon's state dir.
func (m *Manager) tapRecords() map[string]string {
	entries, err := os.ReadDir(m.tokenDir())
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		id, ok := strings.CutSuffix(name, ".tap")
		if !ok {
			continue
		}
		if tap := m.loadTap(id); tap != "" {
			out[id] = tap
		}
	}
	return out
}
