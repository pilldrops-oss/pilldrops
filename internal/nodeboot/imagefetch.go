package nodeboot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/cache"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// imageLoadTimeout bounds docker load only; the fetch is bounded by the caller's context.
const imageLoadTimeout = 10 * time.Minute

// ImageFetcher materialises a published custom image on a docker node by
// fetching the tarball from the artifact store and docker-loading it.
type ImageFetcher struct {
	cache *cache.Cache
	index BuildIndex
	// remote is used directly for metadata.json, which has no header and so
	// cannot go through the cache.
	remote artifact.StorageProvider

	// loading single-flights concurrent creates of one cold image onto one load.
	mu      sync.Mutex
	loading map[string]*imageLoad
}

type imageLoad struct {
	done chan struct{}
	err  error
}

// NewImageFetcher builds the fetcher over an already-opened artifact store.
func NewImageFetcher(remote artifact.StorageProvider, cacheDir string, index BuildIndex, opts cache.Options) (*ImageFetcher, error) {
	if index == nil {
		return nil, errors.New("nodeboot: the image fetcher needs a build index")
	}
	c, err := cache.New(remote, cacheDir, opts)
	if err != nil {
		return nil, err
	}
	return &ImageFetcher{cache: c, index: index, remote: remote, loading: map[string]*imageLoad{}}, nil
}

// Cache exposes the underlying cache, for startup Reclaim.
func (f *ImageFetcher) Cache() *cache.Cache { return f.cache }

// Fetch makes tag available locally. Returns rt.ErrImageNotPublished for tags
// the store has never heard of, which is the ordinary case for registry images
// and which the caller turns into a docker pull. Any other error stops the create.
func (f *ImageFetcher) Fetch(ctx context.Context, tag string) error {
	templateID, ok := artifact.TemplateIDForImageTag(tag)
	if !ok {
		return fmt.Errorf("%w: %s", rt.ErrImageNotPublished, tag)
	}
	buildID, err := f.index.ReadyBuildForTemplate(ctx, templateID)
	if err != nil {
		if errors.Is(err, store.ErrNoReadyBuild) {
			return fmt.Errorf("%w: %s", rt.ErrImageNotPublished, tag)
		}
		return fmt.Errorf("resolve published image %s: %w", tag, err)
	}
	return f.loadOnce(ctx, tag, buildID)
}

// loadOnce collapses concurrent callers for the same tag onto one transfer.
func (f *ImageFetcher) loadOnce(ctx context.Context, tag, buildID string) error {
	f.mu.Lock()
	inflight, running := f.loading[tag]
	if !running {
		inflight = &imageLoad{done: make(chan struct{})}
		f.loading[tag] = inflight
	}
	f.mu.Unlock()

	if running {
		select {
		case <-inflight.done:
			return inflight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	inflight.err = f.load(ctx, tag, buildID)
	f.mu.Lock()
	delete(f.loading, tag)
	f.mu.Unlock()
	close(inflight.done)
	return inflight.err
}

// loadedImageRef pulls what `docker load` says it restored out of its output,
// so a tarball whose recorded name is not the one the build is indexed under
// can still be named. Docker prints one line per image, either
// "Loaded image: <repo>:<tag>" or, for an untagged one, "Loaded image ID:
// sha256:…". The last is taken: a capture tarball holds one image, and if a
// future one held several the most recently written is the whole machine.
// Empty when nothing was recognised, which the caller reports rather than guesses at.
func loadedImageRef(out string) string {
	ref := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"Loaded image ID:", "Loaded image:"} {
			if rest, ok := strings.CutPrefix(line, prefix); ok {
				if rest = strings.TrimSpace(rest); rest != "" {
					ref = rest
				}
				break
			}
		}
	}
	return ref
}

func (f *ImageFetcher) load(ctx context.Context, tag, buildID string) error {
	started := time.Now()
	// Refuse a build whose payload is a Firecracker squashfs before docker load
	// makes something confusing of it.
	if err := checkPayload(ctx, f.remote, buildID, artifact.NameImage); err != nil {
		return fmt.Errorf("image %s: %w", tag, err)
	}
	// Pinned so eviction cannot remove the tarball before docker load reads it.
	src, release, err := f.cache.PathPinned(ctx, buildID, artifact.NameImage)
	if err != nil {
		return fmt.Errorf("fetch image build %s for %s: %w", buildID, tag, err)
	}
	defer release()
	fetched := time.Since(started)

	lctx, cancel := context.WithTimeout(ctx, imageLoadTimeout)
	defer cancel()
	out, err := exec.CommandContext(lctx, "docker", "load", "-i", src).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker load %s (build %s): %w: %s",
			tag, buildID, err, strings.TrimSpace(string(out)))
	}
	// docker load restores whatever tag docker save recorded, which may not be
	// this one. A named snapshot is the case that always differs: its bytes are
	// the checkpoint image `docker commit` wrote, named after the sandbox it was
	// captured from, while the build is indexed under the snapshot's template
	// id. The index is what says which build serves this tag, so the loaded
	// image is named accordingly rather than refused — without it a container
	// node advertises named snapshots, captures one, and cannot boot it.
	if err := exec.CommandContext(lctx, "docker", "image", "inspect", tag).Run(); err != nil {
		loaded := loadedImageRef(string(out))
		if loaded == "" {
			return fmt.Errorf("loaded build %s for %s but no image is tagged %s afterwards "+
				"(docker load said: %s)", buildID, tag, tag, strings.TrimSpace(string(out)))
		}
		if tagOut, tagErr := exec.CommandContext(lctx, "docker", "tag", loaded, tag).CombinedOutput(); tagErr != nil {
			return fmt.Errorf("loaded build %s for %s as %s and could not tag it %s: %w: %s",
				buildID, tag, loaded, tag, tagErr, strings.TrimSpace(string(tagOut)))
		}
	}
	slog.Info("fetched a custom image from the artifact store", "component", "artifact",
		"tag", tag, "build", buildID, "path", src,
		"fetch_ms", fetched.Milliseconds(), "total_ms", time.Since(started).Milliseconds())
	return nil
}
