package store

import (
	"sync"
	"testing"
)

// Concurrent flips on two replicas must each count, and the counter dies with
// its sandbox. Runs on Postgres when SANDBOXD_TEST_POSTGRES_URL is set.
func TestPreviewGenerationCountsEveryFlip(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	if n, err := s.PreviewGeneration(ctx, "sb1"); err != nil || n != 0 {
		t.Fatalf("fresh generation = %d, %v; want 0", n, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.BumpPreviewGeneration(ctx, "sb1") }()
	}
	wg.Wait()
	if n, err := s.PreviewGeneration(ctx, "sb1"); err != nil || n != 8 {
		t.Fatalf("after 8 concurrent bumps = %d, %v; want 8", n, err)
	}
	if err := s.ForgetSandbox(ctx, "sb1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PreviewGeneration(ctx, "sb1"); n != 0 {
		t.Errorf("generation survived its sandbox: %d", n)
	}
}
