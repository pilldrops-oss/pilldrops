package agentd

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// RememberStore persists sandbox-scoped approvals across restarts. Only
// signatures are stored, never call arguments. Denials are never stored; see Gate.Decide.
type RememberStore struct {
	path string
	mu   sync.Mutex
	sigs map[string]bool
}

// OpenRememberStore loads the store at path. A corrupt file starts empty, so the user is asked again.
func OpenRememberStore(path string) *RememberStore {
	s := &RememberStore{path: path, sigs: map[string]bool{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var sigs []string
	if json.Unmarshal(b, &sigs) != nil {
		return s
	}
	for _, sig := range sigs {
		s.sigs[sig] = true
	}
	return s
}

func (s *RememberStore) Has(sig string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sigs[sig]
}

// Add records a signature and flushes it. The error is returned so the caller
// can tell the user rather than silently downgrade to session scope.
func (s *RememberStore) Add(sig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sigs[sig] {
		return nil
	}
	s.sigs[sig] = true
	return s.saveLocked()
}

// Forget removes a signature so a grant can be taken back.
func (s *RememberStore) Forget(sig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sigs[sig] {
		return nil
	}
	delete(s.sigs, sig)
	return s.saveLocked()
}

// List returns the remembered signatures, sorted.
func (s *RememberStore) List() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.sigs))
}

func (s *RememberStore) saveLocked() error {
	b, err := json.Marshal(slices.Sorted(maps.Keys(s.sigs)))
	if err != nil {
		return err
	}
	// Write-then-rename: a torn write would drop every remembered grant.
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
