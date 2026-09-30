package httpapi

import (
	"net/http"
	"strings"
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

// The test server runs the process runtime, which cannot checkpoint. Every
// check the handlers make before reaching the runtime still has to hold, and
// the runtime's refusal must be a 501 naming it, not a retryable error.
func TestCheckpointRoutesOnARuntimeWithoutCheckpoints(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)
	base := srv.URL + "/v1/sandboxes/" + id + "/checkpoints"

	for _, c := range []struct {
		name, method, url, body string
		want                    int
	}{
		{"save on an unknown sandbox", "POST", srv.URL + "/v1/sandboxes/sb-nope/checkpoints", `{"name":"a"}`, 404},
		{"save with a malformed body", "POST", base, `{"name":`, 400},
		{"save without a name", "POST", base, `{}`, 400},
		{"save under a reserved prefix", "POST", base, `{"name":"bkp-1"}`, 400},
		{"list on an unknown sandbox", "GET", srv.URL + "/v1/sandboxes/sb-nope/checkpoints", "", 404},
		{"remove a reserved capture", "DELETE", base + "/export-1", "", 404},
		{"remove one that does not exist", "DELETE", base + "/nope", "", 404},
		{"remove on an unknown sandbox", "DELETE", srv.URL + "/v1/sandboxes/sb-nope/checkpoints/a", "", 404},
		{"restore a reserved capture", "POST", base + "/rst-1/restore", "", 404},
		{"restore on an unknown sandbox", "POST", srv.URL + "/v1/sandboxes/sb-nope/checkpoints/a/restore", "", 404},
	} {
		if code, out := do(t, c.method, c.url, key, c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, code, out, c.want)
		}
	}

	code, out := do(t, "POST", base, key, `{"name":"before-migrate"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("save = %d %v, want 501", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "cannot checkpoint") {
		t.Errorf("save error = %q, want it to say the runtime cannot checkpoint", msg)
	}

	code, out = do(t, "GET", base, key, "")
	if code != 200 {
		t.Fatalf("list = %d %v, want 200", code, out)
	}
	if list, _ := out["checkpoints"].([]any); list == nil || len(list) != 0 {
		t.Errorf("list = %v, want an empty list, never null", out["checkpoints"])
	}
	if max, _ := out["max"].(float64); max <= 0 {
		t.Errorf("list max = %v, want the per-sandbox limit", out["max"])
	}

	if code, out := do(t, "POST", base+"/before-migrate/restore", key, ""); code != http.StatusNotImplemented {
		t.Errorf("restore = %d %v, want 501", code, out)
	}
}

// Another key's sandbox has no checkpoints as far as this key can tell.
func TestCheckpointRoutesAreOwnerScoped(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	id := createSandbox(t, srv.URL, key)
	_, other, err := st.CreateKey(t.Context(), "other", "other-org")
	if err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/v1/sandboxes/" + id + "/checkpoints"
	for _, c := range [][3]string{
		{"POST", base, `{"name":"a"}`}, {"GET", base, ""},
		{"DELETE", base + "/a", ""}, {"POST", base + "/a/restore", ""},
	} {
		if code, _ := do(t, c[0], c[1], other, c[2]); code != 404 {
			t.Errorf("%s %s by another org = %d, want 404", c[0], c[1], code)
		}
	}
}

// The dashboard's charts read these; both answer arrays, never null.
func TestEventsAndConcurrencyAnswerLists(t *testing.T) {
	srv, key := newTestServer(t)
	createSandbox(t, srv.URL, key)

	code, out := do(t, "GET", srv.URL+"/v1/events?limit=5", key, "")
	if code != 200 {
		t.Fatalf("events = %d %v", code, out)
	}
	if _, ok := out["events"].([]any); !ok {
		t.Errorf("events = %v, want a list", out["events"])
	}

	for _, rng := range []string{"5m", "1h", "1d", "bogus"} {
		code, out := do(t, "GET", srv.URL+"/v1/concurrency?range="+rng, key, "")
		if code != 200 {
			t.Fatalf("concurrency %s = %d %v", rng, code, out)
		}
		points, _ := out["points"].([]any)
		if len(points) < 2 {
			t.Fatalf("concurrency %s = %v, want at least the window start and now", rng, out["points"])
		}
		last, _ := points[len(points)-1].(map[string]any)
		if n, _ := last["n"].(float64); n != 1 {
			t.Errorf("concurrency %s ends at %v running, want the 1 sandbox just created", rng, last["n"])
		}
	}
}
