package httpapi

import (
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// How long the guest stood still is the claim a save point makes — it keeps
// running while it is captured — and it was measured on every capture, written
// to disk, and then dropped on the way out. Both the save and the list render
// this body, so both report it.
func TestCheckpointBodyReportsHowLongTheGuestWasPaused(t *testing.T) {
	t.Parallel()
	body := checkpointBody(daemon.Checkpoint{
		Name: "c1", CreatedAt: time.Now(), SizeBytes: 4096,
		Kind: "memory", Status: "ready", PausedMs: 129,
	})
	if body["paused_ms"] != int64(129) {
		t.Fatalf("paused_ms = %v (%T), want 129 — the number is measured and stored "+
			"and nothing could read it", body["paused_ms"], body["paused_ms"])
	}
	// Zero is a fact too (docker commits without stopping the guest), so the
	// field must be present rather than omitted.
	zero := checkpointBody(daemon.Checkpoint{Name: "c2", Kind: "disk", Status: "ready"})
	if _, ok := zero["paused_ms"]; !ok {
		t.Error("paused_ms is missing on a capture that paused for 0 ms; a client " +
			"cannot tell 'not paused' from 'not reported'")
	}
}
