package store

import (
	"fmt"
	"os"
	"slices"
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

func TestEachLifecycleEventQueuesOneDeliveryPerSubscribedHook(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		checkLifecycleEventFanOut(t, openTempStore(t), "acme")
	})

	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL")
		if dsn == "" {
			t.Skip("set SANDBOXD_TEST_POSTGRES_URL to run Postgres tests")
		}
		s, err := Open(t.Context(), dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		defer s.Close()
		org := fmt.Sprintf("fanout-%d", time.Now().UnixNano())
		t.Cleanup(func() {
			for _, table := range []string{"agent_webhook_deliveries", "agent_webhooks", "sandbox_events"} {
				_, _ = s.db.Exec(s.rebind(`DELETE FROM `+table+` WHERE org_id = ?`), org)
			}
		})
		checkLifecycleEventFanOut(t, s, org)
	})
}

func checkLifecycleEventFanOut(t *testing.T, s *Store, org string) {
	t.Helper()
	ctx := t.Context()
	hook := func(events ...string) string {
		t.Helper()
		w, err := s.CreateAgentWebhook(ctx, AgentWebhook{OrgID: org, URL: "https://example.invalid/hook", Events: events}, "secret", 0)
		if err != nil {
			t.Fatalf("create hook: %v", err)
		}
		return w.ID
	}
	want := []string{hook("sandbox.created"), hook("sandbox.created", "sandbox.ended")}
	off := hook("sandbox.created")
	if err := s.SetAgentWebhookEnabled(ctx, org, "", off, false); err != nil {
		t.Fatalf("disable hook: %v", err)
	}
	hook("sandbox.ended")

	// Two of the same event inside one second are two events, not a retry.
	at := time.Now().Unix()
	for range 2 {
		if err := s.RecordEvent(ctx, EventRecord{OrgID: org, SandboxID: "sb-1", Event: "created", Template: "python", At: at}); err != nil {
			t.Fatalf("record event: %v", err)
		}
	}

	got, err := s.ListOrgWebhookDeliveries(ctx, org, 50)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	var hooks []string
	seqs := map[uint64]bool{}
	for _, d := range got {
		hooks = append(hooks, d.WebhookID)
		seqs[d.EventSeq] = true
		if d.EventType != "sandbox.created" || d.State != DeliveryPending || d.SandboxID != "sb-1" {
			t.Errorf("delivery %+v, want a pending sandbox.created for sb-1", d)
		}
		gotAt, template, err := s.SandboxEvent(ctx, org, "sb-1", int64(d.EventSeq))
		if err != nil || gotAt != at || template != "python" {
			t.Errorf("event %d reads back as %d %q (%v), want %d python", d.EventSeq, gotAt, template, err, at)
		}
	}
	want = append(want, want...)
	slices.Sort(hooks)
	slices.Sort(want)
	if !slices.Equal(hooks, want) {
		t.Errorf("deliveries went to %v, want two each (one per event) for the enabled subscribed hooks", hooks)
	}
	if len(seqs) != 2 {
		t.Errorf("the two events share sequence numbers %v, want one each", seqs)
	}
	// A delivery queued before event ids were used carries the event time.
	if gotAt, template, err := s.SandboxEvent(ctx, org, "sb-1", at); err != nil || gotAt != at || template != "python" {
		t.Errorf("an old time-keyed delivery reads back as %d %q (%v), want %d python", gotAt, template, err, at)
	}
}
