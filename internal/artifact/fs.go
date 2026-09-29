package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FS stores artifacts as files under a root directory. It is a production
// backend for single-node installs, but not a shared one: NFS lacks the
// rename atomicity Put relies on.
type FS struct{ root string }

var _ StorageProvider = (*FS)(nil)

// NewFS creates the root if it does not exist.
func NewFS(root string) (*FS, error) {
	if root == "" {
		return nil, errors.New("artifact: filesystem store needs a root directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("artifact: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("artifact: create root %q: %w", abs, err)
	}
	return &FS{root: abs}, nil
}

func (f *FS) Name() string { return "fs" }

// path maps a key to a file path and refuses anything that escapes the root.
// Containment is re-checked after symlink resolution, since a legal key can
// still land outside the root through a symlink.
func (f *FS) path(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	p := filepath.Join(f.root, filepath.FromSlash(key))
	if p != f.root && !strings.HasPrefix(p, f.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q resolves outside the store root", ErrInvalidKey, key)
	}
	if err := f.assertContained(p, key); err != nil {
		return "", err
	}
	return p, nil
}

// assertContained resolves symlinks on the deepest existing ancestor of p
// (the leaf usually does not exist yet on a Put) and verifies it is inside
// the root. The root is resolved too, since on macOS /var is a symlink.
func (f *FS) assertContained(p, key string) error {
	realRoot, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		return fmt.Errorf("artifact: resolve store root: %w", err)
	}
	for cur := p; ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(os.PathSeparator)) {
				return fmt.Errorf("%w: %q resolves to %q, outside the store root", ErrInvalidKey, key, resolved)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("artifact: resolve %q: %w", key, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("%w: %q has no resolvable ancestor", ErrInvalidKey, key)
		}
		cur = parent
	}
}

// Put writes to a temporary file in the destination directory and renames it
// into place. Same directory matters: rename is only atomic within one filesystem.
func (f *FS) Put(ctx context.Context, key string, r io.Reader, _ int64) (err error) {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("artifact: create parent for %q: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-"+filepath.Base(p)+"-*")
	if err != nil {
		return fmt.Errorf("artifact: create temp for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	n, err := io.Copy(&SparseWriter{F: tmp}, newCtxReader(ctx, r))
	if err == nil {
		err = tmp.Truncate(n)
	}
	if err != nil {
		return fmt.Errorf("artifact: write %q: %w", key, err)
	}
	// fsync before rename, or a crash can publish a name whose contents never hit disk.
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("artifact: sync %q: %w", key, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("artifact: close %q: %w", key, err)
	}
	if err = os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("artifact: chmod %q: %w", key, err)
	}
	if err = os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("artifact: publish %q: %w", key, err)
	}
	return nil
}

func (f *FS) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("artifact: open %q: %w", key, err)
	}
	return fh, nil
}

// GetRange returns length bytes from off. A range reaching past the end yields
// what exists (HTTP semantics); one starting past the end is an error.
func (f *FS) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, fmt.Errorf("artifact: negative range for %q (off=%d length=%d)", key, off, length)
	}
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("artifact: open %q: %w", key, err)
	}
	info, err := fh.Stat()
	if err != nil {
		_ = fh.Close()
		return nil, fmt.Errorf("artifact: stat %q: %w", key, err)
	}
	if off > info.Size() {
		_ = fh.Close()
		return nil, fmt.Errorf("artifact: range start %d past end of %q (size %d)", off, key, info.Size())
	}
	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		_ = fh.Close()
		return nil, fmt.Errorf("artifact: seek %q: %w", key, err)
	}
	return readCloser{Reader: io.LimitReader(fh, length), Closer: fh}, nil
}

func (f *FS) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	p, err := f.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("artifact: stat %q: %w", key, err)
	}
	return ObjectInfo{Key: key, Size: info.Size(), Modified: info.ModTime()}, nil
}

func (f *FS) Delete(ctx context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return fmt.Errorf("artifact: delete %q: %w", key, err)
	}
	// Prune now-empty parents; a non-empty one stops the walk.
	for dir := filepath.Dir(p); dir != f.root; dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

// List returns keys under prefix, sorted. In-progress Put temporaries are skipped.
func (f *FS) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(f.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		rel, relErr := filepath.Rel(f.root, p)
		if relErr != nil {
			return relErr
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifact: list %q: %w", prefix, err)
	}
	sort.Strings(keys)
	return keys, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// ctxReader makes a long io.Copy honour context cancellation.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func newCtxReader(ctx context.Context, r io.Reader) io.Reader { return &ctxReader{ctx: ctx, r: r} }

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// FSRoot returns the directory this store writes to.
func (f *FS) FSRoot() string { return f.root }

var _ = time.Time{}
