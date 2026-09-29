package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// unfetchableTag is a name no daemon has and no registry serves, so the store tier is consulted.
func unfetchableTag(t *testing.T) string {
	t.Helper()
	return "sf-test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + ":latest"
}

// A custom image exists in no registry, so the store must be asked first.
func TestTheStoreIsAskedBeforeTheRegistry(t *testing.T) {
	r := &Runtime{}
	var mu sync.Mutex
	var asked []string
	r.SetImageSource(func(ctx context.Context, tag string) error {
		mu.Lock()
		asked = append(asked, tag)
		mu.Unlock()
		return nil // materialised it
	})

	tag := unfetchableTag(t)
	if err := r.ensureImage(t.Context(), tag); err != nil {
		t.Fatalf("ensureImage = %v, want nil: the source reported it had materialised the image", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != tag {
		t.Fatalf("the artifact store was asked for %v, want exactly [%s] — if it was not asked, "+
			"a published custom image is only reachable via a registry it was never in", asked, tag)
	}
}

// Falling through to pull on a store outage would report a registry error for
// a registry that was never involved.
func TestAStoreOutageIsNotReportedAsAMissingImage(t *testing.T) {
	outage := errors.New("dial tcp 127.0.0.1:9090: connect: connection refused")
	r := &Runtime{}
	r.SetImageSource(func(ctx context.Context, tag string) error {
		return fmt.Errorf("fetch image build bld-1 for %s: %w", tag, outage)
	})

	err := r.ensureImage(t.Context(), unfetchableTag(t))
	if err == nil {
		t.Fatal("a store outage produced no error at all")
	}
	if !errors.Is(err, outage) {
		t.Fatalf("ensureImage = %v; the underlying store failure must survive, not be replaced "+
			"by a registry error naming a registry that was never involved", err)
	}
	if strings.Contains(err.Error(), "pull ") {
		t.Errorf("a store outage was reported as a failed pull: %v", err)
	}
}

// Every registry image reaches the store tier on a cold node; not-published is not a failure.
func TestAnUnpublishedImageFallsThroughToTheRegistry(t *testing.T) {
	r := &Runtime{}
	var called int
	var mu sync.Mutex
	r.SetImageSource(func(ctx context.Context, tag string) error {
		mu.Lock()
		called++
		mu.Unlock()
		return fmt.Errorf("%w: %s", rt.ErrImageNotPublished, tag)
	})

	tag := unfetchableTag(t)
	err := r.ensureImage(t.Context(), tag)
	// The pull fails because the tag is not real; the assertion is on which failure.
	if err == nil {
		t.Fatal("want the pull to have been attempted and failed")
	}
	if !strings.Contains(err.Error(), "pull ") {
		t.Fatalf("ensureImage = %v, want a PULL failure — an image the store does not know "+
			"about must fall through to the registry, not be refused", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("the store was asked %d times, want 1", called)
	}
}

func TestWithNoImageSourceNothingChanges(t *testing.T) {
	r := &Runtime{}
	err := r.ensureImage(t.Context(), unfetchableTag(t))
	if err == nil || !strings.Contains(err.Error(), "pull ") {
		t.Fatalf("ensureImage with no source = %v, want the ordinary pull failure", err)
	}
}

func TestAFetchedImageIsNotReFetched(t *testing.T) {
	r := &Runtime{}
	var mu sync.Mutex
	var calls int
	r.SetImageSource(func(ctx context.Context, tag string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})
	tag := unfetchableTag(t)
	for i := 0; i < 3; i++ {
		if err := r.ensureImage(t.Context(), tag); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("the store was asked %d times for 3 creates of one image, want 1", calls)
	}
}
