package store

import (
	"testing"
	"time"
)

func TestADueDeliveryIsClaimedByOneCallerUntilTheLeaseEnds(t *testing.T) {
	st := openTemp(t)
	now := time.Now()
	if _, _, err := st.EnqueueDelivery(t.Context(), AgentWebhookDelivery{
		WebhookID: "whk_1", SandboxID: "sb1", OrgID: "org1",
		EventType: "sandbox.ended", EventSeq: 3, NextAt: now,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claim := func(at time.Time) int {
		t.Helper()
		got, err := st.ClaimDueDeliveries(t.Context(), at, time.Minute, 10, nil)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return len(got)
	}
	if n := claim(now); n != 1 {
		t.Fatalf("first claim got %d deliveries, want 1", n)
	}
	if n := claim(now); n != 0 {
		t.Fatalf("a leased delivery was claimed again (%d)", n)
	}
	if n := claim(now.Add(2 * time.Minute)); n != 1 {
		t.Fatalf("an expired lease was not reclaimed (%d)", n)
	}
}
