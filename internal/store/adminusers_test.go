package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func adminStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOnlyOneFirstAdminCanBeCreated(t *testing.T) {
	st := adminStore(t)

	if err := st.CreateFirstAdmin(t.Context(), "first@example.com", "pbkdf2$1$aa$bb"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	err := st.CreateFirstAdmin(t.Context(), "second@example.com", "pbkdf2$1$cc$dd")
	if !errors.Is(err, ErrAdminExists) {
		t.Fatalf("second claim err = %v, want ErrAdminExists", err)
	}
	n, err := st.AdminCount(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("AdminCount = (%d, %v), want 1", n, err)
	}
}

func TestConcurrentFirstAdminClaimsYieldOne(t *testing.T) {
	st := adminStore(t)

	const claimers = 8
	var wg sync.WaitGroup
	errs := make([]error, claimers)
	start := make(chan struct{})
	for i := range claimers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together
			errs[i] = st.CreateFirstAdmin(t.Context(),
				string(rune('a'+i))+"@example.com", "pbkdf2$1$aa$bb")
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for _, e := range errs {
		if e == nil {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d claims succeeded, want exactly 1", won)
	}
	if n, _ := st.AdminCount(t.Context()); n != 1 {
		t.Errorf("AdminCount = %d, want 1", n)
	}
}

func TestAdminLookupAndPasswordUpdate(t *testing.T) {
	st := adminStore(t)
	if err := st.CreateFirstAdmin(t.Context(), "admin@example.com", "hash-one"); err != nil {
		t.Fatal(err)
	}

	u, found, err := st.AdminByEmail(t.Context(), "admin@example.com")
	if err != nil || !found {
		t.Fatalf("AdminByEmail = (%+v, %v, %v)", u, found, err)
	}
	if u.PassHash != "hash-one" || u.Role != "superadmin" {
		t.Errorf("stored = %+v, want the hash and the superadmin role", u)
	}

	// Unknown address is (false, nil), not an error.
	if _, found, err := st.AdminByEmail(t.Context(), "nobody@example.com"); found || err != nil {
		t.Errorf("unknown address = (%v, %v), want (false, nil)", found, err)
	}

	if err := st.UpdateAdminPassword(t.Context(), "admin@example.com", "hash-two"); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := st.AdminByEmail(t.Context(), "admin@example.com"); u.PassHash != "hash-two" {
		t.Errorf("hash = %q, want the updated one", u.PassHash)
	}
}

func TestListAdminsOmitsHashes(t *testing.T) {
	st := adminStore(t)
	if err := st.CreateFirstAdmin(t.Context(), "admin@example.com", "super-secret-hash"); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListAdmins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d admins, want 1", len(list))
	}
	if list[0].PassHash != "" {
		t.Errorf("ListAdmins returned a password hash: %q", list[0].PassHash)
	}
}
