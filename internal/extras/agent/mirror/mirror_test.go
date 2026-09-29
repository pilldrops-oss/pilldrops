package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func mirrorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// frames renders journal events as agentd writes them: "id: <seq>\ndata: <line>\n\n".
func frames(seqs ...uint64) string {
	var b strings.Builder
	for _, s := range seqs {
		line := fmt.Sprintf(`{"seq":%d,"at":%q,"type":"agent.text","turn_id":"turn_1","payload":{"text":"hi"}}`,
			s, time.Now().UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "id: %d\ndata: %s\n\n", s, line)
	}
	return b.String()
}

// recorder is a Subscribe that hands out canned streams and records each resume cursor.
type recorder struct {
	mu      sync.Mutex
	bodies  map[string][]string // sandbox -> streams, consumed in order
	cursors map[string][]uint64
	calls   int
}

func newRecorder() *recorder {
	return &recorder{bodies: map[string][]string{}, cursors: map[string][]uint64{}}
}

func (r *recorder) sub(ctx context.Context, sandboxID string, since uint64) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.cursors[sandboxID] = append(r.cursors[sandboxID], since)
	queued := r.bodies[sandboxID]
	if len(queued) == 0 {
		// A quiet sandbox is an empty stream, not an error.
		return io.NopCloser(strings.NewReader("")), nil
	}
	body := queued[0]
	r.bodies[sandboxID] = queued[1:]
	return io.NopCloser(strings.NewReader(body)), nil
}

func (r *recorder) cursorsFor(id string) []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]uint64, len(r.cursors[id]))
	copy(out, r.cursors[id])
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMirrorsEventsFromAStream(t *testing.T) {
	st := mirrorStore(t)
	rec := newRecorder()
	rec.bodies["sb1"] = []string{frames(1, 2, 3)}

	m := &Mirror{
		Store:   st,
		Sub:     rec.sub,
		Targets: func() []Target { return []Target{{SandboxID: "sb1", OrgID: "org1"}} },
		Log:     func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer cancel()

	waitFor(t, "3 mirrored events", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 100)
		return len(got) == 3
	})
	got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 100)
	if got[0].Type != "agent.text" || got[0].TurnID != "turn_1" {
		t.Fatalf("event did not round trip: %+v", got[0])
	}
	var p map[string]any
	if err := json.Unmarshal(got[0].Payload, &p); err != nil || p["text"] != "hi" {
		t.Errorf("payload lost: %s (%v)", got[0].Payload, err)
	}
}

func TestResumesFromTheDerivedCursor(t *testing.T) {
	st := mirrorStore(t)
	rec := newRecorder()
	rec.bodies["sb1"] = []string{frames(1, 2), frames(3, 4)}

	m := &Mirror{
		Store:   st,
		Sub:     rec.sub,
		Targets: func() []Target { return []Target{{SandboxID: "sb1", OrgID: "org1"}} },
		Log:     func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer cancel()

	waitFor(t, "all 4 events across two streams", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 100)
		return len(got) == 4
	})

	cursors := rec.cursorsFor("sb1")
	if len(cursors) < 2 {
		t.Fatalf("only %d subscriptions; the reconnect did not happen", len(cursors))
	}
	if cursors[0] != 0 {
		t.Errorf("first subscription resumed from %d, want 0", cursors[0])
	}
	if cursors[1] != 2 {
		t.Errorf("reconnect resumed from %d, want 2 (the highest seq held)", cursors[1])
	}
}

func TestReplayedFramesDoNotDuplicate(t *testing.T) {
	st := mirrorStore(t)
	rec := newRecorder()
	rec.bodies["sb1"] = []string{frames(1, 2, 3), frames(1, 2, 3, 4)}

	m := &Mirror{
		Store:   st,
		Sub:     rec.sub,
		Targets: func() []Target { return []Target{{SandboxID: "sb1", OrgID: "org1"}} },
		Log:     func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer cancel()

	waitFor(t, "seq 4 to arrive", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 3, 100)
		return len(got) == 1
	})
	got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 100)
	if len(got) != 4 {
		t.Fatalf("holding %d events after a replayed range, want 4", len(got))
	}
}

// Asserted in steady state only: between a pause and the next reconcile a follower
// may still retry, which is safe because the daemon refuses with ErrPaused.
func TestAPausedSandboxStopsBeingFollowed(t *testing.T) {
	st := mirrorStore(t)
	rec := newRecorder()
	rec.bodies["sb1"] = []string{frames(1)}

	var running sync.Map
	running.Store("sb1", true)
	m := &Mirror{
		Store: st,
		Sub:   rec.sub,
		Targets: func() []Target {
			if v, _ := running.Load("sb1"); v == true {
				return []Target{{SandboxID: "sb1", OrgID: "org1"}}
			}
			return nil
		},
		Log: func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer cancel()

	waitFor(t, "the first event", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 10)
		return len(got) == 1
	})

	running.Store("sb1", false)
	// Let the reconciler cancel the follower and any in-flight retry finish.
	time.Sleep(2*discoverInterval + 2*reconnectDelay)

	before := len(rec.cursorsFor("sb1"))
	time.Sleep(2*discoverInterval + 2*reconnectDelay)
	if after := len(rec.cursorsFor("sb1")); after != before {
		t.Fatalf("the mirror kept subscribing to a paused sandbox: %d more attempts", after-before)
	}

	rec.bodies["sb1"] = []string{frames(2)}
	running.Store("sb1", true)
	waitFor(t, "mirroring to resume after a resume", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 1, 10)
		return len(got) == 1
	})
	cursors := rec.cursorsFor("sb1")
	if last := cursors[len(cursors)-1]; last != 1 {
		t.Errorf("resumed from %d after the pause, want 1 (the highest seq held)", last)
	}
}

func TestOneUnreachableSandboxDoesNotStopTheOthers(t *testing.T) {
	st := mirrorStore(t)
	rec := newRecorder()
	rec.bodies["ok"] = []string{frames(1, 2)}

	m := &Mirror{
		Store: st,
		Sub: func(ctx context.Context, id string, since uint64) (io.ReadCloser, error) {
			if id == "broken" {
				return nil, fmt.Errorf("agent supervisor is not answering")
			}
			return rec.sub(ctx, id, since)
		},
		Targets: func() []Target {
			return []Target{{SandboxID: "broken", OrgID: "org1"}, {SandboxID: "ok", OrgID: "org1"}}
		},
		Log: func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer cancel()

	waitFor(t, "the healthy sandbox to mirror despite a broken neighbour", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "ok", 0, 10)
		return len(got) == 2
	})
}

func TestKeepalivesAndJunkProduceNoRows(t *testing.T) {
	st := mirrorStore(t)
	rec := newRecorder()
	rec.bodies["sb1"] = []string{
		": keepalive\n\nid: 1\n\ndata: not json\n\ndata: {\"seq\":0,\"type\":\"x\"}\n\n" +
			"data: {\"seq\":5,\"type\":\"\"}\n\n" + frames(9),
	}
	m := &Mirror{
		Store:   st,
		Sub:     rec.sub,
		Targets: func() []Target { return []Target{{SandboxID: "sb1", OrgID: "org1"}} },
		Log:     func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer cancel()

	waitFor(t, "the one real event", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 10)
		return len(got) == 1
	})
	got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 10)
	if got[0].Seq != 9 {
		t.Fatalf("stored seq %d, want only the well-formed event (9)", got[0].Seq)
	}
}

func TestSeqComesFromThePayloadNotTheIdLine(t *testing.T) {
	tgt := Target{SandboxID: "sb1", OrgID: "org1"}
	ev, ok := parseFrame(`data: {"seq":42,"type":"agent.text"}`, tgt)
	if !ok {
		t.Fatal("a well-formed frame was rejected")
	}
	if ev.Seq != 42 {
		t.Fatalf("seq = %d, want 42 from the payload", ev.Seq)
	}
	if _, ok := parseFrame("id: 999", tgt); ok {
		t.Error("an id line produced an event")
	}
}

func TestQuietSandboxIsFlushedOnTimeNotOnTheNextEvent(t *testing.T) {
	st := mirrorStore(t)
	// Two events then a blocked read, so only the timer can flush them.
	held := make(chan struct{})
	body := io.MultiReader(strings.NewReader(frames(1, 2)), readerFunc(func([]byte) (int, error) {
		<-held
		return 0, io.EOF
	}))

	m := &Mirror{
		Store: st,
		Sub: func(context.Context, string, uint64) (io.ReadCloser, error) {
			return io.NopCloser(body), nil
		},
		Targets: func() []Target { return []Target{{SandboxID: "sb1", OrgID: "org1"}} },
		Log:     func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer func() { close(held); cancel() }()

	waitFor(t, "the timer to flush a partial batch", func() bool {
		got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 10)
		return len(got) == 2
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestPermanentFailureStopsFollowing(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	calls := 0

	m := &Mirror{
		Store:   mirrorStore(t),
		Targets: func() []Target { return nil },
		Sub: func(ctx context.Context, id string, since uint64) (io.ReadCloser, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, errPermanentTest
		},
		Permanent: func(err error) bool { return errors.Is(err, errPermanentTest) },
		Log: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(f, a...))
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); m.follow(ctx, Target{SandboxID: "sb1"}) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("follow kept retrying a permanent failure instead of giving up")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("subscribed %d times for a permanent failure, want 1", calls)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d times, want exactly 1: %v", len(logs), logs)
	}
	if !strings.Contains(logs[0], "not mirroring") {
		t.Errorf("the log does not say mirroring stopped: %q", logs[0])
	}
}

func TestRepeatedTransientFailureLogsOnce(t *testing.T) {
	// Several retries must actually happen for "logs once" to mean anything.
	prev := reconnectDelay
	reconnectDelay = 20 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = prev })

	var mu sync.Mutex
	var logs []string
	attempts := 0

	m := &Mirror{
		Store:   mirrorStore(t),
		Targets: func() []Target { return nil },
		Sub: func(ctx context.Context, id string, since uint64) (io.ReadCloser, error) {
			mu.Lock()
			attempts++
			mu.Unlock()
			return nil, errTransientTest
		},
		Log: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(f, a...))
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	m.follow(ctx, Target{SandboxID: "sb1"})

	mu.Lock()
	defer mu.Unlock()
	if attempts < 5 {
		t.Fatalf("only %d attempts in the window — the retry interval was not shortened, so "+
			"this test cannot tell one log from many", attempts)
	}
	if len(logs) != 1 {
		t.Errorf("an unchanging fault logged %d times across %d attempts, want 1: %v",
			len(logs), attempts, logs)
	}
}

var (
	errPermanentTest = errors.New("not supported by this runtime")
	errTransientTest = errors.New("the workstation is paused")
)
