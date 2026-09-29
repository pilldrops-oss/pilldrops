package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// JoinTokenTTL is long enough to boot and install a fresh box, short enough
// that a leaked token is soon useless.
const JoinTokenTTL = 30 * time.Minute

// JoinStore mints and consumes one-time join tokens and allocates unique node names.
type JoinStore interface {
	// Mint returns a fresh one-time token valid until now+ttl.
	Mint(now time.Time, ttl time.Duration) string
	// Consume redeems a token: true exactly once, before it expires.
	Consume(token string, now time.Time) bool
	// AllocateName returns the suggestion if not in taken or previously
	// allocated, else a suffixed variant; "" auto-assigns node-N.
	AllocateName(suggested string, taken []string) string
}

func randToken() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return "sfj_" + hex.EncodeToString(b)
}

type memJoinStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
	names  map[string]struct{}
	seq    int
}

func NewMemJoinStore() *memJoinStore {
	return &memJoinStore{tokens: map[string]time.Time{}, names: map[string]struct{}{}}
}

func (m *memJoinStore) Mint(now time.Time, ttl time.Duration) string {
	tok := randToken()
	m.mu.Lock()
	m.tokens[tok] = now.Add(ttl)
	m.mu.Unlock()
	return tok
}

func (m *memJoinStore) Consume(token string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.tokens[token]
	if !ok {
		return false
	}
	delete(m.tokens, token)
	return now.Before(exp)
}

func (m *memJoinStore) AllocateName(suggested string, taken []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	used := map[string]struct{}{}
	for _, t := range taken {
		used[t] = struct{}{}
	}
	for n := range m.names {
		used[n] = struct{}{}
	}
	free := func(name string) bool { _, bad := used[name]; return !bad }

	var name string
	switch {
	case suggested != "" && free(suggested):
		name = suggested
	case suggested != "":
		for i := 2; ; i++ {
			cand := fmt.Sprintf("%s-%d", suggested, i)
			if free(cand) {
				name = cand
				break
			}
		}
	default:
		for {
			m.seq++
			cand := fmt.Sprintf("node-%d", m.seq)
			if free(cand) {
				name = cand
				break
			}
		}
	}
	m.names[name] = struct{}{}
	return name
}
