package artifact_test

// One conformance suite, run against every backend via its constructor.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// newStore builds an empty store for one test.
type newStore func(t *testing.T) artifact.StorageProvider

func runConformance(t *testing.T, mk newStore) {
	t.Helper()

	t.Run("put then get round-trips", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		body := []byte("template bytes")
		mustPut(t, s, ctx, "b1/rootfs", body)
		if got := mustGet(t, s, ctx, "b1/rootfs"); !bytes.Equal(got, body) {
			t.Fatalf("got %q, want %q", got, body)
		}
	})

	t.Run("put overwrites in place", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		mustPut(t, s, ctx, "b1/rootfs", []byte("first"))
		mustPut(t, s, ctx, "b1/rootfs", []byte("second"))
		if got := mustGet(t, s, ctx, "b1/rootfs"); string(got) != "second" {
			t.Fatalf("got %q, want %q", got, "second")
		}
	})

	t.Run("a missing key is ErrNotFound, not a generic error", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		for _, op := range []struct {
			name string
			fn   func() error
		}{
			{"Get", func() error { _, err := s.Get(ctx, "nope/rootfs"); return err }},
			{"GetRange", func() error { _, err := s.GetRange(ctx, "nope/rootfs", 0, 1); return err }},
			{"Stat", func() error { _, err := s.Stat(ctx, "nope/rootfs"); return err }},
			{"Delete", func() error { return s.Delete(ctx, "nope/rootfs") }},
		} {
			if err := op.fn(); !errors.Is(err, artifact.ErrNotFound) {
				t.Errorf("%s on a missing key = %v, want ErrNotFound", op.name, err)
			}
		}
	})

	t.Run("stat reports the size without reading", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		mustPut(t, s, ctx, "b1/rootfs", bytes.Repeat([]byte("x"), 4096))
		info, err := s.Stat(ctx, "b1/rootfs")
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != 4096 {
			t.Fatalf("size = %d, want 4096", info.Size)
		}
		if info.Key != "b1/rootfs" {
			t.Fatalf("key = %q, want b1/rootfs", info.Key)
		}
	})

	t.Run("ranged reads", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		body := []byte("0123456789")
		mustPut(t, s, ctx, "b1/rootfs", body)

		for _, tc := range []struct {
			off, length int64
			want        string
		}{
			{0, 4, "0123"},
			{3, 4, "3456"},
			{9, 1, "9"},
			{0, 10, "0123456789"},
			// Past the end returns what exists, matching HTTP range semantics.
			{6, 100, "6789"},
		} {
			rc, err := s.GetRange(ctx, "b1/rootfs", tc.off, tc.length)
			if err != nil {
				t.Fatalf("GetRange(%d,%d): %v", tc.off, tc.length, err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if string(got) != tc.want {
				t.Errorf("GetRange(%d,%d) = %q, want %q", tc.off, tc.length, got, tc.want)
			}
		}
	})

	// A start past the end means a stale header; it must not read as an empty block.
	t.Run("a range starting past the end is an error", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		mustPut(t, s, ctx, "b1/rootfs", []byte("short"))
		if _, err := s.GetRange(ctx, "b1/rootfs", 500, 10); err == nil {
			t.Fatal("want an error for a range starting past the end, got nil")
		}
	})

	t.Run("delete removes the object", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		mustPut(t, s, ctx, "b1/rootfs", []byte("x"))
		if err := s.Delete(ctx, "b1/rootfs"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "b1/rootfs"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("after delete, Get = %v, want ErrNotFound", err)
		}
	})

	t.Run("list is prefix-filtered and sorted", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		for _, k := range []string{"b2/rootfs", "b1/rootfs", "b1/metadata.json", "other/x"} {
			mustPut(t, s, ctx, k, []byte("x"))
		}
		got, err := s.List(ctx, "b1/")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"b1/metadata.json", "b1/rootfs"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("List(b1/) = %v, want %v", got, want)
		}
		all, err := s.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 4 {
			t.Fatalf("List(\"\") returned %d keys, want 4: %v", len(all), all)
		}
	})

	// Keys are caller-influenced and become paths on the filesystem backend.
	t.Run("traversal and malformed keys are refused", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		for _, key := range []string{
			"../escape",
			"b1/../../escape",
			"/absolute/key",
			"",
			"b1//rootfs",
			"./hidden",
			".hidden/rootfs",
			"b1/./rootfs",
			"b1/rootfs\x00truncated",
		} {
			if err := s.Put(ctx, key, bytes.NewReader([]byte("x")), 1); err == nil {
				t.Errorf("Put(%q) succeeded; a malformed key must be refused", key)
			}
			if _, err := s.Get(ctx, key); err == nil {
				t.Errorf("Get(%q) succeeded; a malformed key must be refused", key)
			}
		}
	})

	t.Run("a failed put leaves nothing discoverable", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		err := s.Put(ctx, "b1/rootfs", &failingReader{after: 8}, -1)
		if err == nil {
			t.Fatal("want the put to fail")
		}
		if _, err := s.Get(ctx, "b1/rootfs"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("after a failed put, Get = %v, want ErrNotFound — a partial object is visible", err)
		}
		if keys, _ := s.List(ctx, ""); len(keys) != 0 {
			t.Fatalf("after a failed put, List returned %v — a partial object is listable", keys)
		}
	})

	// Distinguishes an atomic publish from write-then-cleanup-on-error, which
	// the failed-put test above cannot.
	t.Run("an in-flight put is not visible", func(t *testing.T) {
		s := mk(t)
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- s.Put(t.Context(), "b1/rootfs", &blockingReader{release: release, body: []byte("complete")}, -1)
		}()

		waitUntil(t, func() bool {
			keys, _ := s.List(t.Context(), "")
			return len(keys) > 0 || blocked(release)
		})
		if _, err := s.Get(t.Context(), "b1/rootfs"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("Get during an in-flight put = %v, want ErrNotFound — a partial object is readable", err)
		}
		if keys, _ := s.List(t.Context(), ""); len(keys) != 0 {
			t.Fatalf("List during an in-flight put returned %v — a partial object is listable", keys)
		}

		close(release)
		if err := <-done; err != nil {
			t.Fatalf("put failed: %v", err)
		}
		if got := mustGet(t, s, t.Context(), "b1/rootfs"); string(got) != "complete" {
			t.Fatalf("after the put completed, got %q", got)
		}
	})

	t.Run("a cancelled context stops a put and publishes nothing", func(t *testing.T) {
		s := mk(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.Put(ctx, "b1/rootfs", bytes.NewReader([]byte("x")), 1); err == nil {
			t.Fatal("want an error from a cancelled put")
		}
		if _, err := s.Get(t.Context(), "b1/rootfs"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("cancelled put left an object: %v", err)
		}
	})

	t.Run("large objects round-trip", func(t *testing.T) {
		s, ctx := mk(t), t.Context()
		body := bytes.Repeat([]byte("abcdefgh"), 1<<17)
		mustPut(t, s, ctx, "b1/rootfs", body)
		if got := mustGet(t, s, ctx, "b1/rootfs"); !bytes.Equal(got, body) {
			t.Fatalf("large object round-trip differs (%d vs %d bytes)", len(got), len(body))
		}
	})

	t.Run("name is non-empty", func(t *testing.T) {
		if mk(t).Name() == "" {
			t.Fatal("a backend must identify itself; the name is recorded in results and logs")
		}
	})
}

func mustPut(t *testing.T, s artifact.StorageProvider, ctx context.Context, key string, body []byte) {
	t.Helper()
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

func mustGet(t *testing.T, s artifact.StorageProvider, ctx context.Context, key string) []byte {
	t.Helper()
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return b
}

// blockingReader yields half of body, waits for release, then yields the rest.
type blockingReader struct {
	release chan struct{}
	body    []byte
	n       int
}

func (b *blockingReader) Read(p []byte) (int, error) {
	half := len(b.body) / 2
	if b.n == half {
		<-b.release
	}
	if b.n >= len(b.body) {
		return 0, io.EOF
	}
	end := min(len(b.body), b.n+max(1, min(len(p), half-b.n)))
	if b.n >= half {
		end = min(len(b.body), b.n+len(p))
	}
	n := copy(p, b.body[b.n:end])
	b.n += n
	return n, nil
}

func blocked(ch chan struct{}) bool {
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// failingReader yields `after` bytes and then fails.
type failingReader struct {
	after int
	n     int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n >= f.after {
		return 0, fmt.Errorf("simulated failure after %d bytes", f.n)
	}
	n := min(len(p), f.after-f.n)
	for i := range n {
		p[i] = 'x'
	}
	f.n += n
	return n, nil
}
