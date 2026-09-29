package agentd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTestJournal(t *testing.T, dir string) *Journal {
	t.Helper()
	j, err := OpenJournal(dir, NewRedactor())
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// collect replays the journal into decoded events.
func collect(t *testing.T, j *Journal, since uint64) []Event {
	t.Helper()
	var out []Event
	err := j.Replay(since, func(seq uint64, line []byte) error {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("bad line at seq %d: %w", seq, err)
		}
		if ev.Seq != seq {
			return fmt.Errorf("replay reported seq %d for an event carrying %d", seq, ev.Seq)
		}
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return out
}

func TestSeqIsMonotonicAndGapFree(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	for i := 0; i < 50; i++ {
		if _, err := j.Append(EventTurnOutput, "turn_1", map[string]any{"text": "x"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	evs := collect(t, j, 0)
	if len(evs) != 50 {
		t.Fatalf("replayed %d events, want 50", len(evs))
	}
	for i, ev := range evs {
		if want := uint64(i + 1); ev.Seq != want {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, want)
		}
	}
	if j.LastSeq() != 50 {
		t.Errorf("LastSeq = %d, want 50", j.LastSeq())
	}
}

func TestSeqIsNeverReusedAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	j1 := openTestJournal(t, dir)
	for i := 0; i < 5; i++ {
		if _, err := j1.Append(EventTurnStarted, "turn_1", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := j1.Close(); err != nil {
		t.Fatal(err)
	}

	j2 := openTestJournal(t, dir)
	if j2.LastSeq() != 5 {
		t.Fatalf("after restart LastSeq = %d, want 5", j2.LastSeq())
	}
	ev, err := j2.Append(EventTurnFinished, "turn_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 6 {
		t.Errorf("first event after restart has seq %d, want 6", ev.Seq)
	}
}

func TestTornFinalLineIsRepairedOnOpen(t *testing.T) {
	dir := t.TempDir()

	j := openTestJournal(t, dir)
	for i := 0; i < 3; i++ {
		if _, err := j.Append(EventTurnOutput, "turn_1", map[string]any{"text": "complete"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// A half-written record with no trailing newline.
	path := filepath.Join(dir, segmentName(1))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":4,"at":"2026-08-07T00:00:00Z","type":"turn.out`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	reopened := openTestJournal(t, dir)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Errorf("torn tail was not truncated (%d -> %d bytes)", before.Size(), after.Size())
	}
	if reopened.LastSeq() != 3 {
		t.Errorf("LastSeq = %d, want 3 (the torn record must not count)", reopened.LastSeq())
	}
	evs := collect(t, reopened, 0)
	if len(evs) != 3 {
		t.Fatalf("replayed %d events after repair, want 3", len(evs))
	}
	ev, err := reopened.Append(EventTurnFinished, "turn_1", nil)
	if err != nil {
		t.Fatalf("append after repair: %v", err)
	}
	if ev.Seq != 4 {
		t.Errorf("post-repair append got seq %d, want 4", ev.Seq)
	}
	if got := collect(t, reopened, 0); len(got) != 4 {
		t.Errorf("after repair + append, replayed %d events, want 4", len(got))
	}
}

func TestReplayReturnsOnlyEventsAfterCursor(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	for i := 0; i < 10; i++ {
		if _, err := j.Append(EventTurnOutput, "turn_1", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	evs := collect(t, j, 7)
	if len(evs) != 3 {
		t.Fatalf("replay since 7 returned %d events, want 3", len(evs))
	}
	if evs[0].Seq != 8 {
		t.Errorf("first replayed seq = %d, want 8", evs[0].Seq)
	}

	if got := collect(t, j, 10); len(got) != 0 {
		t.Errorf("replay from head returned %d events, want 0", len(got))
	}
	if got := collect(t, j, 999); len(got) != 0 {
		t.Errorf("replay past head returned %d events, want 0", len(got))
	}
}

func TestOutputIsCappedWithExactlyOneTruncationMarker(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	j.maxOutput = 1024

	chunk := strings.Repeat("a", 200)
	for i := 0; i < 40; i++ {
		if _, err := j.AppendOutput("turn_1", chunk); err != nil {
			t.Fatalf("append output %d: %v", i, err)
		}
	}

	var outBytes int64
	var markers int
	for _, ev := range collect(t, j, 0) {
		switch ev.Type {
		case EventTurnOutput:
			var p struct{ Text string }
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			outBytes += int64(len(p.Text))
		case EventTurnOutputTruncated:
			markers++
		}
	}
	if outBytes > 1024 {
		t.Errorf("recorded %d bytes of output, cap was 1024", outBytes)
	}
	if markers != 1 {
		t.Errorf("got %d truncation markers, want exactly 1 — one tells the client, thousands bury the turn", markers)
	}
}

func TestOutputBudgetResetsForTheNextTurn(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	j.maxOutput = 512

	if _, err := j.AppendOutput("turn_1", strings.Repeat("a", 900)); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishTurn("turn_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.AppendOutput("turn_2", "fresh turn output"); err != nil {
		t.Fatal(err)
	}

	var turn2 int
	for _, ev := range collect(t, j, 0) {
		if ev.Type == EventTurnOutput && ev.TurnID == "turn_2" {
			turn2++
		}
	}
	if turn2 == 0 {
		t.Error("the next turn inherited the previous turn's exhausted output budget")
	}
}

func TestSecretsAreRedactedBeforeReachingDisk(t *testing.T) {
	dir := t.TempDir()
	red := NewRedactor()
	red.AddSecret("ANTHROPIC_API_KEY", "sk-ant-supersecretvalue-0123456789")
	j, err := OpenJournal(dir, red)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	if _, err := j.AppendOutput("turn_1",
		"exporting ANTHROPIC_API_KEY=sk-ant-supersecretvalue-0123456789 now"); err != nil {
		t.Fatal(err)
	}
	// Nested, so the walk is exercised at depth.
	if _, err := j.Append(EventToolStarted, "turn_1", map[string]any{
		"tool": "Bash",
		"env":  map[string]any{"KEY": "sk-ant-supersecretvalue-0123456789"},
		"argv": []any{"curl", "-H", "Authorization: Bearer sk-ant-supersecretvalue-0123456789"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.Sync(); err != nil {
		t.Fatal(err)
	}

	// Assert against the file; the bytes on disk are what get mirrored.
	raw, err := os.ReadFile(filepath.Join(dir, segmentName(1)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "supersecretvalue") {
		t.Fatal("the secret reached the journal file")
	}
	if !strings.Contains(string(raw), "redacted:ANTHROPIC_API_KEY") {
		t.Errorf("expected a named redaction marker, got:\n%s", raw)
	}
}

func TestReplaySpansSegments(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir)

	// Rotate by hand rather than writing 16 MiB.
	for i := 0; i < 3; i++ {
		if _, err := j.Append(EventTurnOutput, "turn_1", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	j.mu.Lock()
	if err := j.rotateLocked(); err != nil {
		j.mu.Unlock()
		t.Fatal(err)
	}
	j.mu.Unlock()
	for i := 3; i < 6; i++ {
		if _, err := j.Append(EventTurnOutput, "turn_1", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	segs, err := j.segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments, got %v", segs)
	}
	evs := collect(t, j, 0)
	if len(evs) != 6 {
		t.Fatalf("replayed %d events across segments, want 6", len(evs))
	}
	for i, ev := range evs {
		if want := uint64(i + 1); ev.Seq != want {
			t.Errorf("cross-segment event %d has seq %d, want %d", i, ev.Seq, want)
		}
	}

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if got := openTestJournal(t, dir).LastSeq(); got != 6 {
		t.Errorf("LastSeq after multi-segment restart = %d, want 6", got)
	}
}

func TestConcurrentAppendsKeepSeqUnique(t *testing.T) {
	j := openTestJournal(t, t.TempDir())

	const writers, each = 8, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := j.Append(EventTurnOutput, "turn_1", map[string]any{"w": w}); err != nil {
					t.Errorf("concurrent append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	evs := collect(t, j, 0)
	if len(evs) != writers*each {
		t.Fatalf("replayed %d events, want %d", len(evs), writers*each)
	}
	seen := make(map[uint64]bool, len(evs))
	for _, ev := range evs {
		if seen[ev.Seq] {
			t.Fatalf("seq %d was written twice", ev.Seq)
		}
		seen[ev.Seq] = true
	}
	for i := 1; i <= writers*each; i++ {
		if !seen[uint64(i)] {
			t.Fatalf("seq %d is missing — a client resuming there would stall", i)
		}
	}
}

func TestUnknownEventTypesReplayFine(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	if _, err := j.Append("some.future.event", "turn_1", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, j, 0)
	if len(evs) != 1 || evs[0].Type != "some.future.event" {
		t.Fatalf("unknown type did not round trip: %+v", evs)
	}
}

// Run longer with: go test ./internal/extras/agent/agentd/ -run FuzzJournalRecovery -fuzz=Fuzz
func FuzzJournalRecovery(f *testing.F) {
	f.Add([]byte(`{"seq":1,"at":"2026-08-07T00:00:00Z","type":"turn.output"}` + "\n"))
	f.Add([]byte("not json at all\n"))
	f.Add([]byte(`{"seq":`))
	f.Add([]byte("\x00\x00\x00\n\n\n"))
	f.Add([]byte(`{"seq":18446744073709551615,"type":"x"}` + "\n"))
	f.Add([]byte(strings.Repeat("A", 70000) + "\n"))

	f.Fuzz(func(t *testing.T, corrupt []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, segmentName(1)), corrupt, 0o600); err != nil {
			t.Skip()
		}
		// An error is acceptable; a panic is not.
		j, err := OpenJournal(dir, NewRedactor())
		if err != nil {
			return
		}
		defer j.Close()

		_ = j.Replay(0, func(uint64, []byte) error { return nil })

		before := j.LastSeq()
		ev, err := j.Append(EventTurnStarted, "turn_1", map[string]any{"x": "y"})
		if err != nil {
			return
		}
		if ev.Seq <= before {
			t.Fatalf("append reused seq %d after recovering to %d", ev.Seq, before)
		}
	})
}
