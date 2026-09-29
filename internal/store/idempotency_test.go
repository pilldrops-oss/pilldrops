package store

import (
	"bytes"
	"sync"
	"testing"
)

func TestIdempotencyRoundTrip(t *testing.T) {
	st := openTemp(t)

	if _, done, err := st.IdempotentResult(t.Context(), "org1", "k1"); err != nil || done {
		t.Fatalf("missing key: done=%v err=%v, want done=false err=nil", done, err)
	}

	claimed, err := st.ReserveIdempotent(t.Context(), "org1", "k1", []byte(claimInProgress))
	if err != nil || !claimed {
		t.Fatalf("first reserve: claimed=%v err=%v, want claimed=true", claimed, err)
	}

	// An in-flight claim is not reportable as done.
	if _, done, err := st.IdempotentResult(t.Context(), "org1", "k1"); err != nil || done {
		t.Fatalf("in-flight claim: done=%v err=%v, want done=false", done, err)
	}

	body := []byte(`{"id":"sb_abc"}`)
	if err := st.FinishIdempotent(t.Context(), "org1", "k1", body); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, done, err := st.IdempotentResult(t.Context(), "org1", "k1")
	if err != nil || !done || !bytes.Equal(got, body) {
		t.Fatalf("after finish: done=%v err=%v got=%s", done, err, got)
	}

	if _, done, _ := st.IdempotentResult(t.Context(), "org2", "k1"); done {
		t.Fatal("idempotency leaked across orgs")
	}
}

func TestReserveIdempotentPicksOneWinner(t *testing.T) {
	st := openTemp(t)

	const racers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			claimed, err := st.ReserveIdempotent(t.Context(), "org1", "race", []byte(claimInProgress))
			if err != nil {
				return
			}
			if claimed {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d of %d concurrent claimants won, want exactly 1", wins, racers)
	}
}

func TestReleaseIdempotentAllowsRetry(t *testing.T) {
	st := openTemp(t)

	if claimed, _ := st.ReserveIdempotent(t.Context(), "org1", "k", []byte(claimInProgress)); !claimed {
		t.Fatal("want first claim to win")
	}
	if claimed, _ := st.ReserveIdempotent(t.Context(), "org1", "k", []byte(claimInProgress)); claimed {
		t.Fatal("want second claim to lose while the first is held")
	}
	if err := st.ReleaseIdempotent(t.Context(), "org1", "k"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if claimed, _ := st.ReserveIdempotent(t.Context(), "org1", "k", []byte(claimInProgress)); !claimed {
		t.Fatal("want the key claimable again after release")
	}
}
