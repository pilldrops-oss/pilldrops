package orchestrator

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// `sandbox create --mem 999999` answered 503 with Retry-After, and the CLI's
// hint for a 5xx is "server-side error — retry". It will never succeed: no node
// in the fleet is that big, and waiting does not make one. The rest of this
// handler already draws the line — a template nothing hosts, a runtime nothing
// runs and a CPU nothing provides are all 400s naming the reason — and a size
// nothing can hold belongs with them.
func TestASandboxBiggerThanTheFleetIsRefusedNotDeferred(t *testing.T) {
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the node was asked to boot a sandbox larger than itself")
		w.WriteHeader(http.StatusInternalServerError)
	})

	w := doCreate(t, s, `{"template":"base","vcpu":1,"memory_mb":999999}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create with 999999 MB = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After: %s on a refusal retrying can never satisfy", got)
	}
	// The refusal has to say what the ceiling is, or the caller cannot pick a
	// size that would work.
	if body := w.Body.String(); !strings.Contains(body, "999999") || !strings.Contains(body, "MiB") {
		t.Errorf("the refusal names neither the request nor the ceiling: %s", body)
	}
	assertNoLeakedHold(t, s)
}

// The 503 still has to mean what it says. A request no larger than the node,
// which fails only on the headroom the scheduler keeps, is the case where
// waiting is the right advice — and the branch above must not swallow it.
func TestASizeTheFleetCouldHoldStillGetsTheRetryableRefusal(t *testing.T) {
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	w := doCreate(t, s, fmt.Sprintf(`{"template":"base","vcpu":1,"memory_mb":%d}`, fleetNodeMemMB))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a size the node itself has = %d, want 503 (%s)", w.Code, w.Body.String())
	}
}
