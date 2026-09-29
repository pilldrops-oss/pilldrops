package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ReserveSlot is count-then-insert; what makes it atomic differs per dialect
// (Postgres: pg_advisory_xact_lock per org; SQLite: MaxOpenConns(1)), so both
// are exercised. Run with -race.

// reserveConcurrently fires n simultaneous claims against cap and returns how
// many were admitted.
func reserveConcurrently(t *testing.T, s *Store, org string, cap, n int) (admitted int) {
	t.Helper()
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		errs  []error
	)
	start.Add(1)
	for i := 0; i < n; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			ok, err := s.ReserveSlot(t.Context(), org, slotID(org, i), cap)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// Admission fails open on a reservation error, so an error here
				// is an escaped create and must be reported.
				errs = append(errs, err)
				return
			}
			if ok {
				admitted++
			}
		}(i)
	}
	start.Done()
	done.Wait()
	for _, err := range errs {
		t.Errorf("ReserveSlot errored (admission fails OPEN on this, so it is an escaped create): %v", err)
	}
	return admitted
}

func slotID(org string, i int) string {
	return org + "-slot-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}

func TestReserveSlotIsAtomicUnderConcurrency(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		s := openTempStore(t)
		if got, want := reserveConcurrently(t, s, "acme", 3, 20), 3; got != want {
			t.Errorf("20 simultaneous claims against cap=3 admitted %d, want exactly %d", got, want)
		}
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
		// Unique org per run: admission_slots rows outlive the test by 180s and
		// a rerun inside that window would see them and admit 0.
		org := fmt.Sprintf("race-%s-%d", t.Name(), time.Now().UnixNano())
		if got, want := reserveConcurrently(t, s, org, 3, 20), 3; got != want {
			t.Errorf("20 simultaneous claims against cap=3 admitted %d, want exactly %d "+
				"(the advisory lock is what makes count-then-insert safe here)", got, want)
		}
	})
}

func TestReserveSlotCapOfOneIsExactlyOne(t *testing.T) {
	s := openTempStore(t)
	if got := reserveConcurrently(t, s, "solo", 1, 32); got != 1 {
		t.Errorf("32 simultaneous claims against cap=1 admitted %d, want exactly 1", got)
	}
}

func TestReleasedSlotIsReusable(t *testing.T) {
	s := openTempStore(t)
	ok, err := s.ReserveSlot(t.Context(), "reuse", "slot-1", 1)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.ReserveSlot(t.Context(), "reuse", "slot-2", 1); ok {
		t.Fatal("a second claim was admitted while the first was held")
	}
	if err := s.ReleaseSlot(t.Context(), "slot-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, err := s.ReserveSlot(t.Context(), "reuse", "slot-3", 1); err != nil || !ok {
		t.Errorf("after release, a new claim was refused (ok=%v err=%v) — the cap would decay to zero", ok, err)
	}
}

func TestReserveSlotIsPerOrg(t *testing.T) {
	s := openTempStore(t)
	if ok, _ := s.ReserveSlot(t.Context(), "org-a", "a-1", 1); !ok {
		t.Fatal("org-a's first claim was refused")
	}
	if ok, _ := s.ReserveSlot(t.Context(), "org-a", "a-2", 1); ok {
		t.Fatal("org-a exceeded its own cap")
	}
	if ok, err := s.ReserveSlot(t.Context(), "org-b", "b-1", 1); err != nil || !ok {
		t.Errorf("org-b was refused because org-a was full (ok=%v err=%v)", ok, err)
	}
}

// TestUnlimitedCapTakesNoSlot pins that cap <= 0 writes no admission_slots rows.
func TestUnlimitedCapTakesNoSlot(t *testing.T) {
	s := openTempStore(t)
	for i := 0; i < 5; i++ {
		if ok, err := s.ReserveSlot(t.Context(), "unlimited", slotID("u", i), 0); err != nil || !ok {
			t.Fatalf("unlimited claim %d: ok=%v err=%v", i, ok, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admission_slots WHERE org_id = 'unlimited'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("unlimited plan wrote %d admission_slots rows, want 0", n)
	}
}

func openTempStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
