package templates

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// useSource installs a store tier for one test and restores both process-wide
// injection points afterwards.
func useSource(t *testing.T, dir string, fn Source) {
	t.Helper()
	SetDir(dir)
	SetSource(fn)
	t.Cleanup(func() {
		SetSource(nil)
		SetDir("")
		src.mu.Lock()
		src.now = nil
		src.mu.Unlock()
	})
}

// storeWith returns a Source holding the named templates plus a call counter.
func storeWith(held map[string]string) (Source, *atomic.Int64) {
	var calls atomic.Int64
	return func(_ context.Context, id, dir string) error {
		calls.Add(1)
		manifest, ok := held[id]
		if !ok {
			return fmt.Errorf("fetch %q: %w", id, ErrNotInStore)
		}
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("fetched"), 0o644); err != nil {
			return err
		}
		if manifest == "" {
			return nil
		}
		return os.WriteFile(filepath.Join(dir, id, "template.json"), []byte(manifest), 0o644)
	}, &calls
}

func TestStoreTemplateResolvesAndLandsOnDisk(t *testing.T) {
	dir := t.TempDir()
	fetch, calls := storeWith(map[string]string{
		"remote-only": `{"name": "Remote Only", "memory_mb": 3072, "port": 4321}`,
	})
	useSource(t, dir, fetch)

	if _, ok := fromDisk("remote-only"); ok {
		t.Fatal("precondition: remote-only must not be on disk before the fetch")
	}

	got, ok := resolveFetch("remote-only")
	if !ok {
		t.Fatal("a template held only by the store did not resolve")
	}
	if got.Name != "Remote Only" || got.MemoryMB != 3072 || got.Port != 4321 {
		t.Fatalf("fetched template did not come back through the disk tier: %+v", got)
	}

	rootfs := filepath.Join(dir, "remote-only", "rootfs.squashfs")
	if _, err := os.Stat(rootfs); err != nil {
		t.Fatalf("resolving did not leave the template on disk: %v", err)
	}
	var listed bool
	for _, id := range PublishedIDs() {
		if id == "remote-only" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("PublishedIDs() = %v, missing the fetched template", PublishedIDs())
	}

	if _, ok := resolveFetch("remote-only"); !ok {
		t.Fatal("second resolve failed")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("store consulted %d times for a template already on disk, want 1", n)
	}
}

func TestDiskWinsOverTheStore(t *testing.T) {
	dir := t.TempDir()
	fetch, calls := storeWith(map[string]string{
		"desktop": `{"memory_mb": 1024, "name": "Store Copy"}`,
	})
	useSource(t, dir, fetch)
	publish(t, dir, "desktop", `{"memory_mb": 4096, "name": "Local Build"}`)

	got, ok := resolveFetch("desktop")
	if !ok {
		t.Fatal("locally built desktop did not resolve")
	}
	if got.Name != "Local Build" || got.MemoryMB != 4096 {
		t.Fatalf("the store shadowed a locally built template: got %q/%dMB, want %q/4096MB",
			got.Name, got.MemoryMB, "Local Build")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the store was consulted %d times for a template already on disk; "+
			"the store tier must sit UNDER the disk scan, not over it", n)
	}
}

// The call count is the point: a lying source must be cached as a miss, not retried per resolve.
func TestSourceSuccessWithoutARootfsCountsAsAFailedFetch(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	useSource(t, dir, func(_ context.Context, id, dir string) error {
		calls.Add(1)
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, id, "template.json"), []byte(`{"name":"Ghost"}`), 0o644)
	})
	now := time.Now()
	src.mu.Lock()
	src.now = func() time.Time { return now }
	src.mu.Unlock()

	for i := 0; i < 5; i++ {
		if _, ok := resolveFetch("ghost"); ok {
			t.Fatal("a fetch that left no rootfs still resolved")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("source called %d times, want 1: a fetch that leaves no rootfs must "+
			"count as a miss, not as a success to be repeated per request", n)
	}
}

func TestStoreMissIsCachedThenRetried(t *testing.T) {
	dir := t.TempDir()
	fetch, calls := storeWith(nil)
	useSource(t, dir, fetch)

	now := time.Now()
	src.mu.Lock()
	src.now = func() time.Time { return now }
	src.mu.Unlock()

	for i := 0; i < 5; i++ {
		if _, ok := resolveFetch("nowhere"); ok {
			t.Fatal("an id no tier holds resolved")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("store consulted %d times for one unknown id, want 1 (negative cache)", n)
	}

	now = now.Add(missTTL + time.Second)
	if _, ok := resolveFetch("nowhere"); ok {
		t.Fatal("an id no tier holds resolved")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("store consulted %d times after the miss window, want 2 (retry)", n)
	}
}

// The negative cache bounds how often the store is asked; it must not change the answer.
func TestARememberedFetchFailureIsStillAFetchFailure(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	useSource(t, dir, func(context.Context, string, string) error {
		calls.Add(1)
		return errors.New("dial tcp 10.0.0.9:443: connect: connection refused")
	})
	now := time.Now()
	src.mu.Lock()
	src.now = func() time.Time { return now }
	src.mu.Unlock()

	// A compiled-in id, so a miscached failure would fall through and resolve.
	for i := 1; i <= 3; i++ {
		_, err := ResolveFetch(context.Background(), "base")
		if !errors.Is(err, ErrFetchFailed) {
			t.Fatalf("attempt %d: err = %v, want ErrFetchFailed — a store outage is being "+
				"reported as the caller's problem from the second create onwards", i, err)
		}
		if errors.Is(err, ErrUnknownTemplate) {
			t.Fatalf("attempt %d reported an unreachable store as an unknown template", i)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("store consulted %d times in one miss window, want 1 — replaying the error "+
			"must not also mean re-asking", n)
	}

	now = now.Add(missTTL + time.Second)
	if _, err := ResolveFetch(context.Background(), "base"); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("after the window: err = %v, want ErrFetchFailed", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("store consulted %d times after the window, want 2 (retry)", n)
	}
}

func TestConcurrentResolveFetchesOnce(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	release := make(chan struct{})
	useSource(t, dir, func(_ context.Context, id, dir string) error {
		calls.Add(1)
		<-release
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("x"), 0o644)
	})

	const n = 20
	var wg sync.WaitGroup
	ok := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok[i] = resolveFetch("cold")
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, got := range ok {
		if !got {
			t.Fatalf("resolver %d did not see the fetched template", i)
		}
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("%d concurrent resolves caused %d fetches, want 1", n, c)
	}
}

func TestNoSourceLeavesTheOtherTiersAlone(t *testing.T) {
	var calls atomic.Int64
	useSource(t, "", func(_ context.Context, id, dir string) error { calls.Add(1); return nil })

	if _, ok := resolveFetch("desktop"); !ok {
		t.Fatal("compiled-in template stopped resolving")
	}
	if _, ok := resolveFetch("no-such-template"); ok {
		t.Fatal("unknown id resolved")
	}
	if calls.Load() != 0 {
		t.Fatalf("source called %d times with no image directory configured", calls.Load())
	}

	SetSource(nil)
	SetDir(t.TempDir())
	if _, ok := resolveFetch("desktop"); !ok {
		t.Fatal("compiled-in template stopped resolving with the store tier disabled")
	}
}

// resolveFetch is ResolveFetch in (Template, ok) shape; goroutine-safe since it never touches t.
func resolveFetch(id string) (Template, bool) {
	tmpl, err := ResolveFetch(context.Background(), id)
	return tmpl, err == nil
}

func TestResolveNeverFetches(t *testing.T) {
	dir := t.TempDir()
	fetch, calls := storeWith(map[string]string{"remote-only": ""})
	useSource(t, dir, fetch)

	if _, ok := Resolve("remote-only"); ok {
		t.Error("Resolve returned a template only the artifact store holds — it reached the " +
			"store, which puts a multi-hundred-MB download behind every metadata caller")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Resolve consulted the store %d times; it must never consult it at all", n)
	}

	// Positive control: the create path resolves the same id.
	if _, ok := resolveFetch("remote-only"); !ok {
		t.Fatal("ResolveFetch could not resolve the template either — the tier is broken, " +
			"so the assertion above proves nothing")
	}
	if _, ok := Resolve("remote-only"); !ok {
		t.Error("after a fetch, Resolve still cannot see the template — the store tier is " +
			"not populating the disk tier, which is the whole design")
	}
}

func TestFetchFailureIsNotAnUnknownTemplate(t *testing.T) {
	dir := t.TempDir()

	useSource(t, dir, func(_ context.Context, id, _ string) error {
		return errors.New("dial tcp: connection refused")
	})
	_, err := ResolveFetch(context.Background(), "some-custom-image")
	if !errors.Is(err, ErrFetchFailed) {
		t.Errorf("a store that could not be reached gave %v, want ErrFetchFailed", err)
	}
	if errors.Is(err, ErrUnknownTemplate) {
		t.Error("an unreachable store was reported as an unknown template — the caller " +
			"answers 400 and the operator never learns their store is down")
	}

	useSource(t, dir, func(_ context.Context, id, _ string) error { return ErrNotInStore })
	if _, err := ResolveFetch(context.Background(), "no-such-template"); !errors.Is(err, ErrUnknownTemplate) {
		t.Errorf("an id no tier holds gave %v, want ErrUnknownTemplate", err)
	}
	if _, err := ResolveFetch(context.Background(), "desktop"); err != nil {
		t.Errorf("a compiled-in template failed to resolve: %v", err)
	}
}

func TestAWaiterGivesUpOnItsOwnContext(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	useSource(t, dir, func(ctx context.Context, id, _ string) error {
		entered <- struct{}{}
		<-release
		return ErrNotInStore
	})
	t.Cleanup(func() { close(release) })

	go func() { _, _ = ResolveFetch(context.Background(), "slow") }()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { _, err := ResolveFetch(ctx, "slow"); done <- err }()

	select {
	case err := <-done:
		// ErrFetchInProgress rather than context.Canceled: the fetch is no
		// longer this caller's to cancel, and "I gave up" has to reach the
		// tenant as a retryable download rather than as a generic failure.
		if !errors.Is(err, ErrFetchInProgress) {
			t.Errorf("a waiter on a cancelled context returned %v, want ErrFetchInProgress", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter on a cancelled context blocked on the leader's fetch — one caller's " +
			"patience became every caller's")
	}
}

func TestAliasedCreateAsksTheStoreForTheCanonicalID(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var asked []string
	fetch, _ := storeWith(map[string]string{"code-interpreter": ""})
	useSource(t, dir, func(ctx context.Context, id, d string) error {
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		return fetch(ctx, id, d)
	})

	if _, err := ResolveFetch(context.Background(), "python-3.11"); err != nil {
		t.Fatalf("an aliased create did not resolve: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), asked...)
	mu.Unlock()
	for _, id := range got {
		if id != "code-interpreter" {
			t.Errorf("the store was asked for %q; it is keyed by canonical id, so that is a "+
				"guaranteed miss and the create pays a round trip for nothing", id)
		}
	}
	if len(got) != 1 {
		t.Fatalf("the store was consulted %d times for one aliased create: %v", len(got), got)
	}
	if _, err := os.Stat(filepath.Join(dir, "code-interpreter", "rootfs.squashfs")); err != nil {
		t.Errorf("the aliased create left no bootable image on disk: %v", err)
	}
}

func TestAliasOfALocallyHeldTemplateDoesNotFetch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "code-interpreter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code-interpreter", "rootfs.squashfs"),
		[]byte("locally built"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetch, calls := storeWith(map[string]string{"code-interpreter": ""})
	useSource(t, dir, fetch)

	if _, err := ResolveFetch(context.Background(), "python-3.11"); err != nil {
		t.Fatalf("an aliased create of a locally held template failed: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the store was consulted %d times for a template this node already holds — "+
			"every aliased create pays a download it does not need", n)
	}
}
