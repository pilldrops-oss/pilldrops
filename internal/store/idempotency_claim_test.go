package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStaleClaimIsAdoptedSoAPaymentIsNotLost(t *testing.T) {
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	const org, key = "u:payer", "dodo:evt_crash"

	if ok, err := st.ReserveIdempotent(t.Context(), org, key, []byte(claimInProgress)); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	// A fresh in-flight claim must not be overtaken.
	if ok, _ := st.ReserveIdempotent(t.Context(), org, key, []byte(claimInProgress)); ok {
		t.Fatal("a fresh in-flight claim was stolen — that grants twice")
	}

	if _, err := st.exec(t.Context(), `UPDATE idempotency SET at = ? WHERE org_id = ? AND idem_key = ?`,
		time.Now().Unix()-claimStaleSecs-60, org, key); err != nil {
		t.Fatalf("age: %v", err)
	}
	if ok, err := st.ReserveIdempotent(t.Context(), org, key, []byte(claimInProgress)); err != nil || !ok {
		t.Fatalf("stale claim not adopted (%v, %v) — the payment is lost forever", ok, err)
	}
	// Adoption resets the clock.
	if ok, _ := st.ReserveIdempotent(t.Context(), org, key, []byte(claimInProgress)); ok {
		t.Error("the adopted claim was immediately stolen again")
	}
}

func TestFinishedClaimIsNeverAdopted(t *testing.T) {
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "c2.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	const org, key = "u:done", "dodo:evt_ok"

	if ok, _ := st.ReserveIdempotent(t.Context(), org, key, []byte(claimInProgress)); !ok {
		t.Fatal("claim failed")
	}
	if err := st.FinishIdempotent(t.Context(), org, key, []byte("plan:pro")); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := st.exec(t.Context(), `UPDATE idempotency SET at = ? WHERE org_id = ? AND idem_key = ?`,
		time.Now().Unix()-claimStaleSecs*10, org, key); err != nil {
		t.Fatalf("age: %v", err)
	}
	if ok, _ := st.ReserveIdempotent(t.Context(), org, key, []byte(claimInProgress)); ok {
		t.Error("a finished grant was re-claimed — the customer would be granted twice")
	}
}
