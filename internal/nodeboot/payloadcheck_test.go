package nodeboot

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/cache"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// putMetadata writes a build's metadata.json without a header, as publish does.
func putMetadata(t *testing.T, remote artifact.StorageProvider, buildID, templateID, payload string) {
	t.Helper()
	raw, err := artifact.Metadata{
		BuildID: buildID, TemplateID: templateID, Payload: payload,
	}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	key, err := artifact.Key(buildID, artifact.NameMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(t.Context(), key, bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRefusesABuildPublishedForTheOtherRuntime(t *testing.T) {
	fx := newFixture(t, fakeIndex{"sf-acme-tool": "bld-img-1"}, cache.Options{})
	putMetadata(t, fx.remote, "bld-img-1", "sf-acme-tool", artifact.NameImage)

	err := fx.fetcher.Fetch(t.Context(), "sf-acme-tool", fx.imageDir)
	if !errors.Is(err, ErrWrongPayload) {
		t.Fatalf("want ErrWrongPayload, got %v", err)
	}
	for _, want := range []string{artifact.NameImage, artifact.NameRootfs, "bld-img-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q; got %q", want, err)
		}
	}
	// Not-in-store answers 400 to the caller; a wrong-runtime build is an operator problem.
	if errors.Is(err, templates.ErrNotInStore) {
		t.Errorf("a wrong-payload build was reported as not-in-store, which answers 400")
	}
}

func TestImageFetchRefusesATemplateRootfs(t *testing.T) {
	root := t.TempDir()
	remote, err := artifact.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewImageFetcher(remote, t.TempDir(), fakeIndex{"sf-acme-tool": "bld-fc-1"}, cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	putMetadata(t, remote, "bld-fc-1", "sf-acme-tool", artifact.NameRootfs)

	err = f.Fetch(t.Context(), "sf-acme-tool:latest")
	if !errors.Is(err, ErrWrongPayload) {
		t.Fatalf("want ErrWrongPayload, got %v", err)
	}
}

// Older builds have no metadata.json and must still fetch.
func TestFetchAcceptsABuildWithNoMetadata(t *testing.T) {
	body := bytes.Repeat([]byte("squashfs"), 512)
	fx := newFixture(t, fakeIndex{"base": "bld-legacy-1"}, cache.Options{})
	publishRootfs(t, fx.remote, "bld-legacy-1", body)

	if err := fx.fetcher.Fetch(t.Context(), "base", fx.imageDir); err != nil {
		t.Fatalf("a pre-metadata build must still fetch: %v", err)
	}
}

func TestEmptyPayloadMeansRootfs(t *testing.T) {
	root := t.TempDir()
	remote, err := artifact.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	putMetadata(t, remote, "bld-old-1", "base", "")

	if err := checkPayload(context.Background(), remote, "bld-old-1", artifact.NameRootfs); err != nil {
		t.Fatalf("an empty payload must read as %q: %v", artifact.NameRootfs, err)
	}
	if err := checkPayload(context.Background(), remote, "bld-old-1", artifact.NameImage); !errors.Is(err, ErrWrongPayload) {
		t.Fatalf("an empty payload must NOT read as %q: %v", artifact.NameImage, err)
	}
}
