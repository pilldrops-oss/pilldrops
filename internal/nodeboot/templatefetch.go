package nodeboot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/cache"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// BuildIndex resolves a template to its ready build. An interface so the
// fetcher is testable without a database.
type BuildIndex interface {
	ReadyBuildForTemplate(ctx context.Context, templateID string) (string, error)
}

// TemplateFetcher fetches a template's build from the artifact store and
// hard-links it where the vm layer looks.
type TemplateFetcher struct {
	cache *cache.Cache
	index BuildIndex
	// remote is used directly for metadata.json, which has no header and so
	// cannot go through the cache.
	remote artifact.StorageProvider

	mu sync.Mutex
	// builds maps template id to the build this node materialised, so Pin
	// needs no database round trip on the create path.
	builds map[string]string
}

// NewTemplateFetcher builds the fetcher over an already-opened artifact store.
func NewTemplateFetcher(remote artifact.StorageProvider, cacheDir string, index BuildIndex, opts cache.Options) (*TemplateFetcher, error) {
	if index == nil {
		return nil, errors.New("nodeboot: the template fetcher needs a build index")
	}
	c, err := cache.New(remote, cacheDir, opts)
	if err != nil {
		return nil, err
	}
	return &TemplateFetcher{cache: c, index: index, remote: remote, builds: map[string]string{}}, nil
}

// Cache exposes the underlying cache, for startup Reclaim.
func (f *TemplateFetcher) Cache() *cache.Cache { return f.cache }

// Fetch is the templates.Source: materialise template id as
// dir/<id>/rootfs.squashfs. It returns no Template on purpose; the caller
// re-reads disk, so a fetch cannot smuggle in something the vm layer cannot boot.
func (f *TemplateFetcher) Fetch(ctx context.Context, id, dir string) error {
	buildID, err := f.index.ReadyBuildForTemplate(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNoReadyBuild) {
			// Not a failure: the caller falls through to the compiled-in catalog.
			return fmt.Errorf("%w: %s", templates.ErrNotInStore, id)
		}
		return err
	}

	// A named snapshot rides an ordinary template id, so this is the one place
	// that has to tell the two apart before it links anything.
	if meta, ok := buildMetadata(ctx, f.remote, buildID); ok && meta.PayloadName() == artifact.PayloadSnapshot {
		return f.fetchSnapshot(ctx, id, dir, buildID, meta)
	}
	if err := checkPayload(ctx, f.remote, buildID, artifact.NameRootfs); err != nil {
		return fmt.Errorf("template %s: %w", id, err)
	}

	if err := f.link(ctx, buildID, artifact.NameRootfs, filepath.Join(dir, id, "rootfs.squashfs")); err != nil {
		return fmt.Errorf("fetch build %s for template %s: %w", buildID, id, err)
	}

	f.mu.Lock()
	f.builds[id] = buildID
	f.mu.Unlock()

	// Last, and its failure is fatal to the fetch: an image with no stamp is
	// one the resolver will never re-check, so reporting success here would
	// pin this node to this build for good.
	if err := templates.StampBuild(id, buildID); err != nil {
		return fmt.Errorf("record which build %s was materialised from: %w", id, err)
	}
	slog.Info("fetched a template from the artifact store", "component", "artifact",
		"template", id, "build", buildID)
	return nil
}

// CurrentBuild is the templates.CurrentBuild hook: which build this id should
// be serving now. ErrNoReadyBuild is ("", nil) — an id the index never heard of
// is not grounds to distrust what the operator put on disk.
func (f *TemplateFetcher) CurrentBuild(ctx context.Context, id string) (string, error) {
	buildID, err := f.index.ReadyBuildForTemplate(ctx, id)
	if errors.Is(err, store.ErrNoReadyBuild) {
		return "", nil
	}
	return buildID, err
}

// link materialises one object of a build and hard-links it to dst. Pinned
// across the link, so an eviction in between cannot make it fail with a
// misleading ENOENT.
func (f *TemplateFetcher) link(ctx context.Context, buildID, name, dst string) error {
	src, release, err := f.cache.PathPinned(ctx, buildID, name)
	if err != nil {
		return err
	}
	defer release()
	return linkInto(src, dst)
}

// fetchSnapshot materialises a named snapshot: every object of it, root image
// included.
//
// The root image is the part that is easy to get wrong. A snapshot's overlay is
// a delta against one exact read-only disk and its memory image refers into
// that same disk, so a node with the other objects and no root image holds a
// machine it cannot start — and one that substitutes its OWN build of the
// source template holds something worse, a machine that starts and then reads
// EIO from every binary. The image therefore travels as an object of the
// snapshot build, and lands at <dir>/<snap id>/rootfs.squashfs, which is both
// what the restore boots and what makes the snapshot id appear in the disk scan
// the heartbeat advertises. "This node holds the snapshot" and "this node has
// everything the snapshot needs" stay the same fact.
// fetchSnapshotObject lands one object of a named snapshot in its capture
// directory.
func (f *TemplateFetcher) fetchSnapshotObject(ctx context.Context, id, dir, buildID, source, name string) error {
	// Landed under the engine's own file names, not the store's: what this
	// directory has to be is a capture directory the restore can read, and
	// the store's names are an artifact-layer concern that stops here.
	err := f.link(ctx, buildID, name, filepath.Join(dir, id, snapshotFileName(name)))
	if err == nil {
		return nil
	}
	// A build published before the root image travelled with the capture.
	// Fall back to what those builds have always relied on: this node's own
	// copy of the source template. It is only correct when the two nodes
	// built the same bytes, which is why the image travels now — but the
	// restore checks the digest recorded in the capture and refuses rather
	// than booting something broken, so the fallback cannot go quiet.
	if name == artifact.NameRootfs && errors.Is(err, artifact.ErrNotFound) {
		if err := f.linkSourceTemplateRootfs(ctx, id, dir, source); err != nil {
			return err
		}
		slog.Warn("a named snapshot carries no root image of its own; falling back to this "+
			"node's build of the source template, which restores only if the two are identical",
			"component", "artifact", "snapshot", id, "build", buildID, "source_template", source)
		return nil
	}
	return fmt.Errorf("fetch %s of snapshot %s (build %s): %w", name, id, buildID, err)
}

func (f *TemplateFetcher) fetchSnapshot(ctx context.Context, id, dir, buildID string, meta artifact.Metadata) error {
	if meta.Snapshot == nil || meta.Snapshot.Template == "" {
		return fmt.Errorf("snapshot build %s does not say which template it was captured from, "+
			"so its overlay has no backing disk", buildID)
	}
	source := meta.Snapshot.Template
	// Downloaded in parallel: each object is bound by its sha256 on one core
	// (~8 s per GiB on an Ivy node), so a memfile and an overlay one after
	// another took the sum and side by side take the larger. Pinned until
	// linked, so the cache cannot evict one in between; a failure here is
	// reported by the link below, where a missing rootfs has its fallback.
	var mu sync.Mutex
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	var wg sync.WaitGroup
	for _, name := range artifact.SnapshotObjects() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, release, err := f.cache.PathPinned(ctx, buildID, name); err == nil {
				mu.Lock()
				releases = append(releases, release)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Linked one at a time in SnapshotObjects' order, the root image last: a
	// directory holding rootfs.squashfs is a template the create path boots,
	// and linked as each download finished, the rootfs landed before the
	// memory file and a create polling meanwhile booted a capture without one.
	for _, name := range artifact.SnapshotObjects() {
		if err := f.fetchSnapshotObject(ctx, id, dir, buildID, source, name); err != nil {
			return err
		}
	}

	f.mu.Lock()
	f.builds[id] = buildID
	f.mu.Unlock()

	// See Fetch: the stamp is what lets `snapshot save <name>` a second time
	// reach a node that already holds the first version.
	if err := templates.StampBuild(id, buildID); err != nil {
		return fmt.Errorf("record which build snapshot %s was materialised from: %w", id, err)
	}

	// ponytail: a deleted snapshot leaves this directory behind, so the node
	// keeps advertising the id. Harmless — the control plane resolves the name
	// before it places anything, so a create from a deleted snapshot is a 404
	// and never reaches a node — but it costs an inode set and a stale entry in
	// the heartbeat. Sweep it when the overlay cleaner grows a template pass.
	slog.Info("fetched a named snapshot from the artifact store", "component", "artifact",
		"snapshot", id, "build", buildID, "source_template", source,
		"vcpu", meta.Snapshot.VCPU, "mem_mb", meta.Snapshot.MemMB)
	return nil
}

// linkSourceTemplateRootfs puts this node's build of the snapshot's source
// template where the restore looks, fetching the template first if this node
// has never held it. Only for snapshot builds published before the image
// travelled with the capture.
func (f *TemplateFetcher) linkSourceTemplateRootfs(ctx context.Context, id, dir, source string) error {
	sourceRootfs := filepath.Join(dir, source, "rootfs.squashfs")
	if _, err := os.Stat(sourceRootfs); err != nil {
		// Fetched through the ordinary template path, so it is cached,
		// advertised and evicted like any other template.
		if err := f.Fetch(ctx, source, dir); err != nil {
			return fmt.Errorf("snapshot %s was captured from template %s, whose rootfs this node "+
				"does not hold and could not fetch (%w) — the overlay has no backing disk",
				id, source, err)
		}
	}
	return linkInto(sourceRootfs, filepath.Join(dir, id, "rootfs.squashfs"))
}

// Pin holds the artifacts a running sandbox of this template depends on until
// the returned function is called. A no-op for templates this node never fetched.
func (f *TemplateFetcher) Pin(templateID string) func() {
	f.mu.Lock()
	buildID := f.builds[templateID]
	f.mu.Unlock()
	if buildID == "" {
		return func() {}
	}
	return f.cache.Acquire(buildID)
}

// linkInto hard-links src to dst, replacing whatever is at dst. Link-then-rename
// so the path never disappears while a concurrent create stats it.
func linkInto(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create image directory for %s: %w", dst, err)
	}
	tmp := dst + ".fetch-link"
	_ = os.Remove(tmp) // leftover from a crash between Link and Rename
	if err := os.Link(src, tmp); err != nil {
		return fmt.Errorf("link the fetched artifact into the image directory "+
			"(%s -> %s): %w", src, dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish the fetched artifact at %s: %w", dst, err)
	}
	return nil
}

// CacheDir is where fetched artifacts are materialised. Not
// SANDBOXD_ARTIFACT_DIR: with the fs backend that directory is the store itself.
func CacheDir(dataDir string) string {
	return filepath.Join(dataDir, "artifact-cache")
}

// ArtifactTiers is what InstallTemplateSource wired up.
type ArtifactTiers struct {
	// PinTemplate holds a running sandbox's template artifacts. Never nil.
	PinTemplate func(string) func()
	// FetchImage materialises a published custom image for docker. Nil when
	// this node has no artifact store.
	FetchImage func(context.Context, string) error
	// CanFetch is advertised in the heartbeat as CanFetchTemplates.
	CanFetch bool
	// Backups is the object store sandbox backups are written to, nil when
	// this node's store cannot hold them. The control-plane store is refused
	// on purpose: it is Postgres or a directory behind the orchestrator, and a
	// multi-gigabyte disk pushed through it would hit the bandwidth ceiling
	// that object storage exists to lift. A node without one answers 501 to a
	// backup rather than quietly filling the control plane.
	Backups artifact.StorageProvider
}

// backupCapableStore reports whether a store kind can hold a disk image.
func backupCapableStore(k artifact.Kind) bool {
	return k == artifact.KindFS || k == artifact.KindS3
}

// InstallTemplateSource opens this node's artifact store and installs the
// lazy-fetch tiers: custom images for docker nodes, and templates for nodes
// with an image directory.
func InstallTemplateSource(ctx context.Context, dataDir, orchURL, nodeKey string, index BuildIndex, saved *artifact.S3Options) (ArtifactTiers, error) {
	noop := func(string) func() { return func() {} }
	out := ArtifactTiers{PinTemplate: noop}

	remote, cfg, err := OpenArtifactStore(dataDir, orchURL, nodeKey, nil, saved)
	if err != nil {
		return out, err
	}
	out.CanFetch = artifact.Shared(cfg.Kind)
	if backupCapableStore(cfg.Kind) {
		out.Backups = remote
	}
	// Said at boot, not at the first backup request: a default clustered node
	// takes no backups at all and a default standalone node takes them onto the
	// disk it is backing up, and neither is visible from anywhere else.
	if w := BackupStoreWarning(cfg); w != "" {
		slog.Warn(w, "component", "artifact", "store", string(cfg.Kind), "clustered", cfg.Clustered)
	}

	cacheDir := CacheDir(dataDir)
	// 0 means unbounded; the platform does not guess a disk bound.
	maxMB := int64(EnvInt("SANDBOXD_ARTIFACT_CACHE_MAX_MB", 0))
	opts := cache.Options{MaxBytes: maxMB << 20}

	imgFetcher, err := NewImageFetcher(remote, cacheDir, index, opts)
	if err != nil {
		return out, err
	}
	reclaim(ctx, imgFetcher.Cache(), cacheDir)
	out.FetchImage = imgFetcher.Fetch

	imageDir := templates.Dir()
	if imageDir == "" {
		slog.Info("artifact store ready; custom-image fetch installed, no image directory so no template fetch tier",
			"component", "artifact", "kind", string(cfg.Kind), "clustered", cfg.Clustered,
			"can_fetch_templates", out.CanFetch, "cache_dir", cacheDir)
		return out, nil
	}
	if err := LinkProbeError(cacheDir, imageDir); err != nil {
		return out, err
	}
	f, err := NewTemplateFetcher(remote, cacheDir, index, opts)
	if err != nil {
		return out, err
	}
	out.PinTemplate = f.Pin

	templates.SetSource(f.Fetch)
	templates.SetCurrentBuild(f.CurrentBuild)
	slog.Info("artifact store ready; lazy template and custom-image fetch installed", "component", "artifact",
		"kind", string(cfg.Kind), "clustered", cfg.Clustered, "can_fetch_templates", out.CanFetch,
		"cache_dir", cacheDir, "image_dir", imageDir, "cache_max_mb", maxMB)
	return out, nil
}

// reclaim drops fetch temporaries a crash left behind.
func reclaim(ctx context.Context, c *cache.Cache, dir string) {
	if n, err := c.Reclaim(ctx); err != nil {
		slog.Warn("artifact cache reclaim failed; accounting may be stale until restart",
			"component", "artifact", "dir", dir, "err", err)
	} else if n > 0 {
		slog.Info("artifact cache reclaimed incomplete fetches",
			"component", "artifact", "removed", n)
	}
}

// LinkProbeError checks at boot that the artifact cache can hard-link into the
// image directory, so an EXDEV surfaces now rather than on the first cold fetch.
func LinkProbeError(artifactDir, imageDir string) error {
	if artifactDir == "" || imageDir == "" {
		return nil // disk discovery is off
	}
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return fmt.Errorf("SANDBOXD_ARTIFACT_DIR %q could not be created: %w", artifactDir, err)
	}
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("the image directory %q could not be created: %w", imageDir, err)
	}
	probe, err := os.CreateTemp(artifactDir, ".linkprobe-*")
	if err != nil {
		return fmt.Errorf("SANDBOXD_ARTIFACT_DIR %q is not writable: %w", artifactDir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	defer func() { _ = os.Remove(name) }()

	dst := filepath.Join(imageDir, filepath.Base(name))
	if err := os.Link(name, dst); err != nil {
		return fmt.Errorf(
			"this node cannot hard-link from its artifact cache into its image directory "+
				"(%s -> %s): %w. A fetched template is linked rather than copied so one "+
				"artifact occupies disk once and the cache stays the thing that evicts it, "+
				"which needs both paths on the same filesystem. Point SANDBOXD_ARTIFACT_DIR "+
				"and SNAPSHOT_DIR at the same volume",
			artifactDir, imageDir, err)
	}
	_ = os.Remove(dst)
	return nil
}

// snapshotFileName maps a snapshot object's store name to the file name the
// capture engine wrote and the restore reads. The node's export API holds the
// same mapping in the other direction (internal/httpapi/snapshotexport.go); the
// two are the only places the store's vocabulary and the engine's meet.
func snapshotFileName(object string) string {
	switch object {
	case artifact.NameSnapfile:
		return "state"
	case artifact.NameMemfile:
		return "mem"
	case artifact.NameOverlay:
		return "overlay.ext4"
	case artifact.NameSnapMeta:
		return "meta.json"
	case artifact.NameRootfs:
		return "rootfs.squashfs"
	}
	return object
}
