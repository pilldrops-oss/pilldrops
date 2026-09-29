package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func mirrorStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func ev(sb string, seq uint64, typ string) MirroredEvent {
	return MirroredEvent{
		SandboxID: sb, Seq: seq, OrgID: "org1", TurnID: "turn_1", Type: typ,
		At: time.Now(), Payload: json.RawMessage(`{"text":"hello"}`),
	}
}

func TestReplayAfterRestartIsANoOp(t *testing.T) {
	st := mirrorStore(t)
	batch := []MirroredEvent{ev("sb1", 1, "turn.started"), ev("sb1", 2, "agent.text"), ev("sb1", 3, "turn.finished")}

	n, err := st.MirrorEvents(t.Context(), batch)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if n != 3 {
		t.Fatalf("inserted %d, want 3", n)
	}

	n, err = st.MirrorEvents(t.Context(), batch)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 0 {
		t.Fatalf("replay inserted %d rows, want 0 — the timeline now has duplicates", n)
	}

	got, err := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("holding %d events after a replay, want 3", len(got))
	}
	// An overlapping batch inserts only what is new.
	n, err = st.MirrorEvents(t.Context(), []MirroredEvent{ev("sb1", 3, "turn.finished"), ev("sb1", 4, "session.ready")})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("overlapping batch inserted %d, want 1", n)
	}
}

func TestCursorIsDerivedFromTheRowsItDescribes(t *testing.T) {
	st := mirrorStore(t)

	if c, err := st.MirrorCursor(t.Context(), "sb1"); err != nil || c != 0 {
		t.Fatalf("empty cursor = (%d, %v), want (0, nil)", c, err)
	}
	if _, err := st.MirrorEvents(t.Context(), []MirroredEvent{ev("sb1", 1, "a"), ev("sb1", 7, "b")}); err != nil {
		t.Fatal(err)
	}
	c, err := st.MirrorCursor(t.Context(), "sb1")
	if err != nil {
		t.Fatal(err)
	}
	if c != 7 {
		t.Fatalf("cursor = %d, want 7 (the highest seq held)", c)
	}
	// Per sandbox, not global.
	if c, _ := st.MirrorCursor(t.Context(), "sb2"); c != 0 {
		t.Fatalf("sb2 cursor = %d, want 0", c)
	}
}

func TestMirroredEventsResumeOnTheSameCursorAsTheLiveStream(t *testing.T) {
	st := mirrorStore(t)
	if _, err := st.MirrorEvents(t.Context(), []MirroredEvent{
		ev("sb1", 1, "a"), ev("sb1", 2, "b"), ev("sb1", 3, "c"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListMirroredEvents(t.Context(), "org1", "sb1", 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("since_seq=2 returned %d events (first seq %v), want just seq 3",
			len(got), func() any {
				if len(got) > 0 {
					return got[0].Seq
				}
				return nil
			}())
	}
	if _, err := st.MirrorEvents(t.Context(), []MirroredEvent{ev("sb1", 4, "d")}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 100)
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("events are not ascending: %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
}

func TestMirrorSkipsMalformedEventsWithoutFailingTheBatch(t *testing.T) {
	st := mirrorStore(t)
	n, err := st.MirrorEvents(t.Context(), []MirroredEvent{
		ev("sb1", 1, "good"),
		{SandboxID: "", Seq: 2, Type: "no sandbox"},
		{SandboxID: "sb1", Seq: 0, Type: "seq zero is not a value the journal issues"},
		{SandboxID: "sb1", Seq: 3, Type: ""},
		ev("sb1", 4, "also good"),
	})
	if err != nil {
		t.Fatalf("one bad row failed the whole batch: %v", err)
	}
	if n != 2 {
		t.Fatalf("inserted %d, want the 2 well-formed events", n)
	}
}

func TestARunningTurnDoesNotEraseAFinishedTurnsCost(t *testing.T) {
	st := mirrorStore(t)
	cost := 0.42
	base := MirroredTurn{
		ID: "turn_1", SandboxID: "sb1", OrgID: "org1", Request: "do it",
		State: "done", CostUSD: &cost, QueuedAt: time.Now(),
	}
	if err := st.MirrorTurn(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	base.CostUSD = nil
	base.Response = "finished"
	if err := st.MirrorTurn(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListMirroredTurns(t.Context(), "org1", "sb1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].CostUSD == nil || *got[0].CostUSD != 0.42 {
		t.Fatalf("cost = %v after a nil-cost re-observation, want 0.42 kept", got[0].CostUSD)
	}
	// The rest of the record still advances.
	if got[0].Response != "finished" {
		t.Errorf("response was not updated: %q", got[0].Response)
	}
}

func TestUnreportedCostStaysNull(t *testing.T) {
	st := mirrorStore(t)
	if err := st.MirrorTurn(t.Context(), MirroredTurn{
		ID: "turn_1", SandboxID: "sb1", OrgID: "org1", State: "done", QueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListMirroredTurns(t.Context(), "org1", "sb1", 10)
	if got[0].CostUSD != nil {
		t.Fatalf("cost = %v, want nil — no interpolated estimates", *got[0].CostUSD)
	}
}

func TestMirrorReadsAreOrgScoped(t *testing.T) {
	st := mirrorStore(t)
	if _, err := st.MirrorEvents(t.Context(), []MirroredEvent{ev("sb1", 1, "a")}); err != nil {
		t.Fatal(err)
	}
	if err := st.MirrorTurn(t.Context(), MirroredTurn{
		ID: "turn_1", SandboxID: "sb1", OrgID: "org1", QueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ListMirroredEvents(t.Context(), "org2", "sb1", 0, 10); len(got) != 0 {
		t.Errorf("another org read %d mirrored events", len(got))
	}
	if got, _ := st.ListMirroredTurns(t.Context(), "org2", "sb1", 10); len(got) != 0 {
		t.Errorf("another org read %d mirrored turns", len(got))
	}
}

func TestPruneDropsEventsAndKeepsTurns(t *testing.T) {
	st := mirrorStore(t)
	old := ev("sb1", 1, "old")
	old.At = time.Now().Add(-60 * 24 * time.Hour)
	recent := ev("sb1", 2, "recent")
	if _, err := st.MirrorEvents(t.Context(), []MirroredEvent{old, recent}); err != nil {
		t.Fatal(err)
	}
	if err := st.MirrorTurn(t.Context(), MirroredTurn{
		ID: "turn_1", SandboxID: "sb1", OrgID: "org1",
		QueuedAt: time.Now().Add(-60 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := st.PruneMirror(t.Context(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d events, want 1", n)
	}
	got, _ := st.ListMirroredEvents(t.Context(), "org1", "sb1", 0, 10)
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("the wrong event was pruned: %+v", got)
	}
	if turns, _ := st.ListMirroredTurns(t.Context(), "org1", "sb1", 10); len(turns) != 1 {
		t.Fatal("pruning events also dropped the turn record")
	}
}

func TestPruneLeavesUndatedEventsAlone(t *testing.T) {
	st := mirrorStore(t)
	undated := ev("sb1", 1, "undated")
	undated.At = time.Time{}
	if _, err := st.MirrorEvents(t.Context(), []MirroredEvent{undated}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PruneMirror(t.Context(), time.Now()); err != nil || n != 0 {
		t.Fatalf("pruned %d undated events (err %v), want 0", n, err)
	}
}
