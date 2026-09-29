// Package mirror copies an agent sandbox's event journal into the control plane
// so history outlives the sandbox. The resume point is store.MirrorCursor, the
// max seq already written, so nothing here tracks progress.
package mirror

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	// discoverInterval is how often the mirror looks for sandboxes to follow.
	discoverInterval = 2 * time.Second

	// flushInterval and flushSize bound how long an event waits in memory.
	flushInterval = 1 * time.Second
	flushSize     = 200

	// maxFrameBytes bounds one SSE frame; journal lines carry file contents.
	maxFrameBytes = 8 << 20

	// reconnectDelayDefault is the pause before re-following a sandbox whose stream ended.
	reconnectDelayDefault = 2 * time.Second
)

// reconnectDelay is a var only so tests can shorten it.
var reconnectDelay = reconnectDelayDefault

// Target is one sandbox the mirror should follow.
type Target struct {
	SandboxID string
	OrgID     string
}

// Subscribe opens an event stream for a sandbox from a cursor.
type Subscribe func(ctx context.Context, sandboxID string, sinceSeq uint64) (io.ReadCloser, error)

// Mirror follows agent sandboxes and writes their events to the store.
type Mirror struct {
	Store *store.Store
	// Targets must return only running sandboxes; reading a paused one would wake
	// it and bill the customer.
	Targets func() []Target
	Sub     Subscribe
	// Permanent reports an error that will never resolve for this sandbox, so the
	// follower stops. Nil treats every failure as transient.
	Permanent func(error) bool
	// Log is optional.
	Log func(format string, args ...any)

	mu        sync.Mutex
	following map[string]context.CancelFunc
}

// Run follows sandboxes until ctx is cancelled.
func (m *Mirror) Run(ctx context.Context) {
	m.following = map[string]context.CancelFunc{}
	defer m.stopAll()

	t := time.NewTicker(discoverInterval)
	defer t.Stop()
	for {
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcile starts followers for new targets and stops those that are gone.
func (m *Mirror) reconcile(ctx context.Context) {
	want := map[string]Target{}
	for _, t := range m.Targets() {
		if t.SandboxID != "" {
			want[t.SandboxID] = t
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.following {
		if _, ok := want[id]; !ok {
			cancel()
			delete(m.following, id)
		}
	}
	for id, tgt := range want {
		if _, ok := m.following[id]; ok {
			continue
		}
		fctx, cancel := context.WithCancel(ctx)
		m.following[id] = cancel
		go m.follow(fctx, tgt)
	}
}

func (m *Mirror) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.following {
		cancel()
		delete(m.following, id)
	}
}

func (m *Mirror) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log(format, args...)
		return
	}
	log.Printf(format, args...)
}

// follow keeps one sandbox mirrored until its context ends. A repeating fault is
// logged once, not once per retry.
func (m *Mirror) follow(ctx context.Context, tgt Target) {
	last := ""
	for ctx.Err() == nil {
		if err := m.followOnce(ctx, tgt); err != nil && ctx.Err() == nil {
			if m.Permanent != nil && m.Permanent(err) {
				m.logf("[mirror] %s: not mirroring this sandbox — %v", tgt.SandboxID, err)
				return
			}
			if msg := err.Error(); msg != last {
				last = msg
				m.logf("[mirror] %s: %v", tgt.SandboxID, err)
			}
		} else if err == nil {
			// Recovered, so the next fault is reported even if it is the same one.
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// followOnce holds one subscription and mirrors what arrives.
func (m *Mirror) followOnce(ctx context.Context, tgt Target) error {
	// Re-derived on every reconnect; a cached cursor would be a second source of truth.
	cursor, err := m.Store.MirrorCursor(ctx, tgt.SandboxID)
	if err != nil {
		return err
	}

	body, err := m.Sub(ctx, tgt.SandboxID, cursor)
	if err != nil {
		return err
	}
	defer body.Close()

	batch := make([]store.MirroredEvent, 0, flushSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if _, err := m.Store.MirrorEvents(ctx, batch); err != nil {
			// Drop rather than grow unbounded; the journal still has these events and
			// the derived cursor re-reads them on reconnect.
			m.logf("[mirror] %s: dropping a batch of %d, will re-read on reconnect: %v",
				tgt.SandboxID, len(batch), err)
		}
		batch = batch[:0]
	}

	// A ticker, so a quiet sandbox's last events do not wait for the next one.
	tick := time.NewTicker(flushInterval)
	defer tick.Stop()
	done := make(chan struct{})
	var mu sync.Mutex
	go func() {
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				mu.Lock()
				flush()
				mu.Unlock()
			}
		}
	}()
	defer func() {
		close(done)
		mu.Lock()
		flush()
		mu.Unlock()
	}()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	for sc.Scan() {
		ev, ok := parseFrame(sc.Text(), tgt)
		if !ok {
			continue
		}
		mu.Lock()
		batch = append(batch, ev)
		if len(batch) >= flushSize {
			flush()
		}
		mu.Unlock()
	}
	return sc.Err()
}

// parseFrame reads one SSE data line into an event. The seq comes from the
// journal line itself, so the SSE id line is ignored.
func parseFrame(line string, tgt Target) (store.MirroredEvent, bool) {
	data, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return store.MirroredEvent{}, false
	}
	var e struct {
		Seq     uint64          `json:"seq"`
		At      time.Time       `json:"at"`
		Type    string          `json:"type"`
		TurnID  string          `json:"turn_id"`
		Step    int             `json:"step"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal([]byte(data), &e) != nil || e.Seq == 0 || e.Type == "" {
		// A row without a seq would make the derived cursor lie.
		return store.MirroredEvent{}, false
	}
	return store.MirroredEvent{
		SandboxID: tgt.SandboxID, OrgID: tgt.OrgID, Seq: e.Seq,
		TurnID: e.TurnID, Type: e.Type, Step: e.Step, At: e.At, Payload: e.Payload,
	}, true
}
