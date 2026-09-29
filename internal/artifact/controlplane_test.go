package artifact_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// Runs the shared conformance suite through a real Handler over a real FS store.
func TestControlPlaneConformance(t *testing.T) {
	t.Parallel()
	runConformance(t, func(t *testing.T) artifact.StorageProvider {
		t.Helper()
		return newCPStore(t, "test-node-key")
	})
}

func newCPStore(t *testing.T, key string) artifact.StorageProvider {
	t.Helper()
	backing, err := artifact.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	h := artifact.NewHandler(backing)

	// Mounted behind a credential check, the way the control plane mounts it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	cp, err := artifact.NewControlPlane(srv.URL, key, srv.Client())
	if err != nil {
		t.Fatalf("NewControlPlane: %v", err)
	}
	return cp
}

func TestControlPlaneRequiresACredential(t *testing.T) {
	t.Parallel()
	s := newCPStore(t, "right-key")
	if err := s.Put(t.Context(), "b1/rootfs", strings.NewReader("x"), 1); err != nil {
		t.Fatalf("seed: %v", err)
	}

	backing, _ := artifact.NewFS(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer right-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		artifact.NewHandler(backing).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	wrong, err := artifact.NewControlPlane(srv.URL, "wrong-key", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Get(t.Context(), "b1/rootfs"); err == nil {
		t.Fatal("a wrong credential could read an artifact")
	}
}

func TestNewControlPlaneRejectsBadInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, url, key string }{
		{"no url", "", "k"},
		{"no credential", "http://cp", ""},
		{"not a url", "://nope", "k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := artifact.NewControlPlane(tc.url, tc.key, nil); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestOpenRefusesUnknownKinds(t *testing.T) {
	t.Parallel()
	if _, err := artifact.Open("nonsense", artifact.Options{}); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
	if s, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()}); err != nil || s.Name() != "fs" {
		t.Fatalf("Open(fs) = %v, %v", s, err)
	}
}

// FS is not shared even on a network mount: NFS lacks the atomicity Put relies on.
func TestSharedClassification(t *testing.T) {
	t.Parallel()
	if artifact.Shared(artifact.KindFS) {
		t.Error("fs must not count as shared; a fleet guard accepting it would accept the failure it prevents")
	}
	for _, k := range []artifact.Kind{artifact.KindControlPlane, artifact.KindS3} {
		if !artifact.Shared(k) {
			t.Errorf("%q should count as shared", k)
		}
	}
}

func TestCopyBetweenBackends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	src := newCPStore(t, "k")
	dst, err := artifact.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"b1/rootfs", "b1/metadata.json", "b2/rootfs"} {
		if err := src.Put(ctx, k, strings.NewReader("payload-"+k), int64(len("payload-"+k))); err != nil {
			t.Fatal(err)
		}
	}

	n, err := artifact.Copy(ctx, src, dst, "b1/")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 2 {
		t.Fatalf("copied %d, want 2 (the prefix must be honoured)", n)
	}
	got, err := dst.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "b1/metadata.json,b1/rootfs" {
		t.Fatalf("destination holds %v", got)
	}
}
