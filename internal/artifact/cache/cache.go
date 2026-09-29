// Package cache fetches artifacts once, verifies them against their header,
// and keeps them as local files (Firecracker needs a path, not a reader)
// under a disk high-water mark. Pinned artifacts are never evicted.
package cache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
)

// DefaultTTL is how long an unused artifact is kept. It must exceed the
// maximum sandbox lifetime (24h) so nothing expires under a running sandbox.
const DefaultTTL = 25 * time.Hour

// Cache fetches artifacts from a remote store and keeps them on local disk.
type Cache struct {
	remote artifact.StorageProvider
	dir    string

	maxBytes int64
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*entry // local key -> entry
	stats   Stats
}

type entry struct {
	key      string // the artifact key, e.g. "bld-1/rootfs"
	size     int64
	lastUsed time.Time
	// pins counts sandboxes depending on this artifact; pinned entries are never evicted.
	pins int
}

// Stats is what the node publishes about its cache.
type Stats struct {
	Hits          int64
	Misses        int64
	BytesFetched  int64
	Evictions     int64
	ResidentBytes int64
	Entries       int
}

// Options configures a Cache. Zero values are sensible.
type Options struct {
	// MaxBytes is the high-water mark for resident artifacts. Zero means unbounded.
	MaxBytes int64
	// TTL defaults to DefaultTTL.
	TTL time.Duration
	// Now is injectable so eviction and expiry are testable without sleeping.
	Now func() time.Time
}

// New opens a cache over dir, fetching from remote.
func New(remote artifact.StorageProvider, dir string, opts Options) (*Cache, error) {
	if remote == nil {
		return nil, errors.New("cache: needs a remote store to fetch from")
	}
	if dir == "" {
		return nil, errors.New("cache: needs a local directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("cache: resolve %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("cache: create %q: %w", abs, err)
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{
		remote:   remote,
		dir:      abs,
		maxBytes: opts.MaxBytes,
		ttl:      opts.TTL,
		now:      opts.Now,
		entries:  map[string]*entry{},
	}, nil
}

// localPath maps an artifact key to its file, re-checking containment since
// the key becomes a path here.
func (c *Cache) localPath(key string) (string, error) {
	if err := artifact.ValidateKey(key); err != nil {
		return "", err
	}
	p := filepath.Join(c.dir, filepath.FromSlash(key))
	if !strings.HasPrefix(p, c.dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q resolves outside the cache", artifact.ErrInvalidKey, key)
	}
	return p, nil
}

// Path returns a local file path for an artifact, fetching and verifying it
// first if needed. The path is not pinned, so eviction may remove it at any
// time; callers that boot from or link the file must use PathPinned.
func (c *Cache) Path(ctx context.Context, buildID, name string) (string, error) {
	p, release, err := c.PathPinned(ctx, buildID, name)
	if err != nil {
		return "", err
	}
	release()
	return p, nil
}

// PathPinned is Path with the artifact already pinned on return. The caller
// releases once the path no longer needs to stay valid. Acquire cannot pin
// before the fetch completes, so the pin has to be taken here.
func (c *Cache) PathPinned(ctx context.Context, buildID, name string) (string, func(), error) {
	key, err := artifact.Key(buildID, name)
	if err != nil {
		return "", nil, err
	}
	p, err := c.localPath(key)
	if err != nil {
		return "", nil, err
	}

	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if _, statErr := os.Stat(p); statErr == nil {
			e.lastUsed = c.now()
			c.stats.Hits++
			e.pins++
			c.mu.Unlock()
			return p, c.releaseOnce(e), nil
		}
		// Tracked but gone from disk; re-fetch rather than return a dead path.
		delete(c.entries, key)
		c.stats.ResidentBytes -= e.size
	}
	c.stats.Misses++
	c.mu.Unlock()

	started := time.Now()
	size, err := c.fetchVerified(ctx, buildID, name, key, p)
	if err != nil {
		return "", nil, err
	}
	slog.Info("fetched an artifact object", "component", "artifact", "key", key,
		"bytes", size, "seconds", time.Since(started).Seconds())

	c.mu.Lock()
	// Born pinned so the eviction sweep below cannot pick the file just fetched.
	e := &entry{key: key, size: size, lastUsed: c.now(), pins: 1}
	c.entries[key] = e
	c.stats.ResidentBytes += size
	c.stats.BytesFetched += size
	c.stats.Entries = len(c.entries)
	c.mu.Unlock()

	c.evictIfOver(ctx)
	return p, c.releaseOnce(e), nil
}

// releaseOnce returns a function that drops one pin on e, idempotently.
func (c *Cache) releaseOnce(e *entry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if e.pins > 0 {
				e.pins--
			}
		})
	}
}

// fetchVerified downloads to a temporary file, checks the digest against the
// artifact's header, and only then renames it into place. A missing header
// is a refusal, not a fallback to trusting the bytes.
func (c *Cache) fetchVerified(ctx context.Context, buildID, name, key, dst string) (int64, error) {
	hdr, err := c.fetchHeader(ctx, buildID, name)
	if err != nil {
		return 0, err
	}
	if !hdr.Whole() {
		return c.fetchPacked(ctx, hdr, name, key, dst)
	}
	want, ok := hdr.SelfChecksum()
	if !ok {
		return 0, fmt.Errorf("cache: header for %q carries no checksum for its own build", key)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("cache: create parent for %q: %w", key, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".fetch-"+filepath.Base(dst)+"-*")
	if err != nil {
		return 0, fmt.Errorf("cache: temp file for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	// Best-effort; runs only on paths that are already failing.
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	rc, err := c.remote.Get(ctx, key)
	if err != nil {
		cleanup()
		return 0, err
	}
	defer rc.Close()

	// Hashed inline: an io.Pipe to a hashing goroutine measured twice as slow
	// (17.8 s vs 8.8 s per GiB on an Ivy node), and the hash is 8 s of the 8.8.
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(&artifact.SparseWriter{F: tmp}, sum), rc)
	if err == nil {
		err = tmp.Truncate(n) // a trailing hole has no bytes written to set the length
	}
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("cache: fetch %q: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, fmt.Errorf("cache: sync %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return 0, fmt.Errorf("cache: close %q: %w", key, err)
	}

	var got [sha256.Size]byte
	copy(got[:], sum.Sum(nil))
	if got != want {
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("cache: %q failed verification: header says %x, fetched %x — "+
			"the local copy has been discarded", key, want[:8], got[:8])
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("cache: publish %q: %w", key, err)
	}
	return n, nil
}

func (c *Cache) fetchHeader(ctx context.Context, buildID, name string) (*header.Header, error) {
	hkey, err := artifact.HeaderKey(buildID, name)
	if err != nil {
		return nil, err
	}
	rc, err := c.remote.Get(ctx, hkey)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return nil, fmt.Errorf("cache: %q has no header, so its contents cannot be verified — "+
				"refusing to use an unverifiable artifact: %w", hkey, err)
		}
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("cache: read header %q: %w", hkey, err)
	}
	return header.Unmarshal(raw)
}

// Acquire pins every resident artifact of a build until the returned function
// is called. The sandbox lifecycle uses it to keep a live VM's rootfs resident.
func (c *Cache) Acquire(buildID string) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := buildID + "/"
	var pinned []*entry
	for k, e := range c.entries {
		if strings.HasPrefix(k, prefix) {
			e.pins++
			pinned = append(pinned, e)
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			for _, e := range pinned {
				if e.pins > 0 {
					e.pins--
				}
			}
		})
	}
}

// evictIfOver removes expired entries, then least-recently-used ones until
// resident bytes are under the high-water mark. Pinned entries are skipped.
func (c *Cache) evictIfOver(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := c.now().Add(-c.ttl)
	candidates := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		if e.pins == 0 {
			candidates = append(candidates, e)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].lastUsed.Before(candidates[j].lastUsed) })

	for _, e := range candidates {
		expired := e.lastUsed.Before(cutoff)
		overWater := c.maxBytes > 0 && c.stats.ResidentBytes > c.maxBytes
		if !expired && !overWater {
			break
		}
		if err := ctx.Err(); err != nil {
			return
		}
		c.removeLocked(e)
	}
}

func (c *Cache) removeLocked(e *entry) {
	p, err := c.localPath(e.key)
	if err == nil {
		_ = os.Remove(p)
	}
	delete(c.entries, e.key)
	c.stats.ResidentBytes -= e.size
	c.stats.Evictions++
	c.stats.Entries = len(c.entries)
}

// Stats returns a snapshot.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = len(c.entries)
	return s
}

// Reclaim scans the cache directory at startup, removes leftover fetch
// temporaries, and rebuilds the in-memory accounting.
func (c *Cache) Reclaim(ctx context.Context) (removed int, err error) {
	c.mu.Lock()
	c.entries = map[string]*entry{}
	c.stats.ResidentBytes = 0
	c.mu.Unlock()

	walkErr := filepath.WalkDir(c.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".fetch-") {
			if os.Remove(p) == nil {
				removed++
			}
			return nil
		}
		rel, relErr := filepath.Rel(c.dir, p)
		if relErr != nil {
			return relErr
		}
		key := filepath.ToSlash(rel)
		// Headers are accounted with the data they describe.
		if strings.HasSuffix(key, artifact.HeaderSuffix) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		c.mu.Lock()
		c.entries[key] = &entry{key: key, size: info.Size(), lastUsed: c.now()}
		c.stats.ResidentBytes += info.Size()
		c.mu.Unlock()
		return nil
	})
	if walkErr != nil {
		return removed, fmt.Errorf("cache: reclaim: %w", walkErr)
	}
	c.mu.Lock()
	c.stats.Entries = len(c.entries)
	c.mu.Unlock()
	return removed, nil
}

// Dir returns the cache directory.
func (c *Cache) Dir() string { return c.dir }

// fetchPacked rebuilds an artifact stored packed or chained onto other builds
// (artifact.GetPacked, which verifies every build it reads), zero runs as holes,
// into a temporary file renamed into place only once it is whole.
func (c *Cache) fetchPacked(ctx context.Context, hdr *header.Header, name, key, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("cache: create parent for %q: %w", key, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".fetch-"+filepath.Base(dst)+"-*")
	if err != nil {
		return 0, fmt.Errorf("cache: temp file for %q: %w", key, err)
	}
	keyOf := func(buildID string) (string, error) { return artifact.Key(buildID, name) }
	err = artifact.GetPacked(ctx, c.remote, keyOf, hdr, tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, fmt.Errorf("cache: fetch %q: %w", key, err)
	}
	return int64(hdr.TotalSize), nil
}
