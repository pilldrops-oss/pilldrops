package agentd

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestBudget(t *testing.T) *Budget {
	t.Helper()
	b, err := OpenBudget(filepath.Join(t.TempDir(), "budget.json"))
	if err != nil {
		t.Fatalf("open budget: %v", err)
	}
	return b
}

func f(v float64) *float64 { return &v }

func TestUnreportedCostIsCountedNotTreatedAsZero(t *testing.T) {
	b := openTestBudget(t)
	b.SetCap(1.00)

	for i := 0; i < 5; i++ {
		if crossed := b.Record(nil); len(crossed) != 0 {
			t.Errorf("an unreported turn crossed a threshold: %v", crossed)
		}
	}
	snap := b.Snapshot()
	if snap.SpentUSD != 0 {
		t.Errorf("spent = %v, want 0 — nothing was reported", snap.SpentUSD)
	}
	if snap.TurnsUnreported != 5 {
		t.Errorf("turns_unreported = %d, want 5", snap.TurnsUnreported)
	}
	if snap.Exceeded {
		t.Error("unreported turns exceeded the cap; unknown is not the same as spent")
	}
	if snap.Coverage == "" {
		t.Error("no coverage note, so $0.00 alongside 5 unreported turns reads as free")
	}
}

func TestThresholdsFireOnceEachInOrder(t *testing.T) {
	b := openTestBudget(t)
	b.SetCap(10.00)

	if crossed := b.Record(f(4.99)); len(crossed) != 0 {
		t.Errorf("crossed %v below 50%%", crossed)
	}
	if crossed := b.Record(f(0.02)); len(crossed) != 1 || crossed[0] != 50 {
		t.Errorf("crossing 50%% gave %v", crossed)
	}
	if crossed := b.Record(f(0.01)); len(crossed) != 0 {
		t.Errorf("50%% announced twice: %v", crossed)
	}
	if crossed := b.Record(f(3.0)); len(crossed) != 1 || crossed[0] != 80 {
		t.Errorf("crossing 80%% gave %v", crossed)
	}
	if crossed := b.Record(f(2.0)); len(crossed) != 1 || crossed[0] != 100 {
		t.Errorf("crossing 100%% gave %v", crossed)
	}
	if !b.Exceeded() {
		t.Error("cap not exceeded after spending past it")
	}
}

func TestOneLargeTurnReportsEveryThresholdItCrossed(t *testing.T) {
	b := openTestBudget(t)
	b.SetCap(1.00)
	crossed := b.Record(f(1.50))
	if len(crossed) != 3 {
		t.Fatalf("crossed %v, want 50, 80 and 100", crossed)
	}
}

func TestNoCapMeansUnlimited(t *testing.T) {
	b := openTestBudget(t)
	if crossed := b.Record(f(1000)); len(crossed) != 0 {
		t.Errorf("thresholds fired with no cap set: %v", crossed)
	}
	snap := b.Snapshot()
	if snap.Exceeded {
		t.Error("exceeded with no cap set")
	}
	if snap.RemainingUSD != nil {
		t.Errorf("remaining = %v, want absent when there is no cap", *snap.RemainingUSD)
	}
}

func TestLoweringTheCapBelowSpendExceedsImmediately(t *testing.T) {
	b := openTestBudget(t)
	b.SetCap(10.00)
	b.Record(f(4.00))
	if b.Exceeded() {
		t.Fatal("exceeded too early")
	}
	snap := b.SetCap(2.00)
	if !snap.Exceeded {
		t.Error("lowering the cap under current spend did not exceed it")
	}
}

func TestRaisingTheCapReArmsThresholds(t *testing.T) {
	b := openTestBudget(t)
	b.SetCap(1.00)
	b.Record(f(1.00)) // fires 50, 80, 100
	b.SetCap(10.00)

	crossed := b.Record(f(0.01)) // 1.01 of 10, below 50%
	if len(crossed) != 0 {
		t.Errorf("crossed %v immediately after raising the cap", crossed)
	}
	if crossed := b.Record(f(4.0)); len(crossed) != 1 || crossed[0] != 50 {
		t.Errorf("50%% of the NEW cap was not announced: %v", crossed)
	}
}

func TestBudgetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")

	b1, err := OpenBudget(path)
	if err != nil {
		t.Fatal(err)
	}
	b1.SetCap(5.00)
	b1.Record(f(3.00))
	b1.Record(nil)

	b2, err := OpenBudget(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := b2.Snapshot()
	if snap.CapUSD != 5.00 || snap.SpentUSD != 3.00 {
		t.Errorf("after restart cap/spent = %v/%v, want 5/3", snap.CapUSD, snap.SpentUSD)
	}
	if snap.TurnsUnreported != 1 {
		t.Errorf("unreported count lost across restart: %d", snap.TurnsUnreported)
	}
	// Thresholds already fired must not re-fire after a restart.
	if crossed := b2.Record(f(0.01)); len(crossed) != 0 {
		t.Errorf("restart re-announced %v", crossed)
	}
}

func TestCorruptBudgetFileStartsCleanRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	if err := writeFileHelper(path, "{not json"); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBudget(path)
	if err != nil {
		t.Fatalf("a corrupt budget file made the supervisor unopenable: %v", err)
	}
	if b.Exceeded() {
		t.Error("a corrupt file was read as an exceeded cap, locking the workstation")
	}
}

func TestExceededCapFreezesTheQueueWithoutFailingTheTurn(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `printf ok` })
	sup.Budget = openTestBudget(t)
	sup.Budget.SetCap(1.00)
	sup.Budget.Record(f(1.00)) // exhausted

	if _, err := sup.Submit("blocked", ModeAutonomous, "latest", "api", false); err != ErrSpendCapReached {
		t.Fatalf("submit with an exhausted cap = %v, want ErrSpendCapReached", err)
	}

	// A turn already queued before the cap ran out stays queued.
	turn, err := sup.Queue.Enqueue("queued earlier", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	stop := runLoop(t, sup)
	defer stop()

	waitFor(t, "the session to report the freeze", func() bool {
		return sup.Status().State == StateDegraded
	})
	if reason := sup.Status().DegradedReason; reason == "" {
		t.Error("frozen with no reason; the workstation just looks broken")
	}
	got, err := sup.Queue.Get(turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TurnQueued {
		t.Errorf("turn state = %s, want it still queued so raising the cap runs it", got.State)
	}

	sup.Budget.SetCap(10.00)
	sup.Recovered()
	sup.Notify()
	waitFor(t, "the turn to run after the cap was raised", func() bool {
		got, err := sup.Queue.Get(turn.ID)
		return err == nil && got.State == TurnDone
	})
}

func TestThresholdCrossingIsJournaled(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `printf ok` })
	sup.Budget = openTestBudget(t)
	sup.Budget.SetCap(1.00)

	sup.recordSpend("turn_x", f(0.60))

	var found bool
	for _, ev := range collect(t, sup.Journal, 0) {
		if ev.Type == EventSpendThreshold {
			found = true
			p := payload(t, ev)
			if p["percent"] != float64(50) {
				t.Errorf("percent = %v, want 50", p["percent"])
			}
			if p["coverage"] == "" {
				t.Error("the event omits what the cap does not cover")
			}
		}
	}
	if !found {
		t.Error("no spend.threshold event was journaled")
	}
}

// 402 rather than 429: retrying changes nothing, a decision does.
func TestSubmitReturns402WhenTheCapIsReached(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `printf ok` })
	sup.Budget = openTestBudget(t)
	sup.Budget.SetCap(1.00)
	sup.Budget.Record(f(1.00))

	srv := &Server{Sup: sup, Journal: sup.Journal, Queue: sup.Queue}
	ts := newServerFor(t, srv)

	resp, body := req(t, ts, "POST", "/turns", `{"message":"anything"}`, "")
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("submit with an exhausted cap = %d, want 402", resp.StatusCode)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("402 carries no explanation")
	}
}

func TestBudgetEndpointsRoundTripAndUnfreeze(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `printf ok` })
	sup.Budget = openTestBudget(t)
	srv := &Server{Sup: sup, Journal: sup.Journal, Queue: sup.Queue}
	ts := newServerFor(t, srv)

	_, body := req(t, ts, "GET", "/budget", "", "")
	if body["enabled"] != true {
		t.Fatalf("budget not enabled: %v", body)
	}

	resp, body := req(t, ts, "PATCH", "/budget", `{"cap_usd":2.5}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d", resp.StatusCode)
	}
	b, _ := body["budget"].(map[string]any)
	if b["cap_usd"] != 2.5 {
		t.Errorf("cap = %v, want 2.5", b["cap_usd"])
	}

	// cap_usd is required; defaulting to 0 would clear the cap on a malformed request.
	if resp, _ := req(t, ts, "PATCH", "/budget", `{}`, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PATCH with no cap_usd = %d, want 400", resp.StatusCode)
	}

	sup.Budget.Record(f(5.00))
	sup.degrade(spendFrozenReason)
	if _, _ = req(t, ts, "PATCH", "/budget", `{"cap_usd":100}`, ""); sup.Status().State == StateDegraded {
		t.Error("raising the cap left the workstation frozen")
	}
}

func TestNoBudgetConfiguredReportsDisabled(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `printf ok` })
	srv := &Server{Sup: sup, Journal: sup.Journal, Queue: sup.Queue} // Budget nil
	ts := newServerFor(t, srv)

	_, body := req(t, ts, "GET", "/budget", "", "")
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if reason, _ := body["reason"].(string); reason == "" {
		t.Error("no reason given for a disabled budget")
	}
	if resp, _ := req(t, ts, "PATCH", "/budget", `{"cap_usd":1}`, ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("PATCH with no store = %d, want 501", resp.StatusCode)
	}
	if sup.Status().Spend != nil {
		t.Error("status reports a spend figure with no budget configured")
	}
	_ = time.Now
}

func writeFileHelper(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
