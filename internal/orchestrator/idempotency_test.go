package orchestrator

import (
	"testing"
)

func TestTheKeyIsStableForOneRequest(t *testing.T) {
	ctx := withIdempotencyKey(t.Context(), "customer-chosen-key-1")
	first, second := idempotencyKey(ctx), idempotencyKey(ctx)
	if first == "" {
		t.Fatal("no key on the context")
	}
	if first != second {
		t.Fatalf("key changed between reads (%q then %q) — a retry would not dedup", first, second)
	}
	other := withIdempotencyKey(t.Context(), "customer-chosen-key-2")
	if idempotencyKey(other) == first {
		t.Fatal("two distinct requests were given the same key")
	}
}

func TestACallerSuppliedKeyIsNotOverwritten(t *testing.T) {
	const mine = "customer-chosen-key-1"
	if got := idempotencyKey(withIdempotencyKey(t.Context(), mine)); got != mine {
		t.Fatalf("idempotencyKey = %q, want the caller's %q", got, mine)
	}
}
