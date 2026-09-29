// Package artifact is the storage plane for template root filesystems and
// snapshots. Artifacts are self-describing so a node can fetch, verify and
// boot one without the database, which owns visibility and ownership.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned by Get, GetRange, Stat and Delete for a missing key.
	ErrNotFound = errors.New("artifact not found")

	// ErrInvalidKey is returned for a key ValidateKey rejects.
	ErrInvalidKey = errors.New("invalid artifact key")
)

// ObjectInfo is what a store knows about an object without reading it.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

// StorageProvider is the blob-store contract every backend implements.
type StorageProvider interface {
	// Put stores r under key atomically: a failed or interrupted Put leaves
	// nothing discoverable under key. size is the expected length, or -1.
	Put(ctx context.Context, key string, r io.Reader, size int64) error

	// Get returns the whole object. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// GetRange returns length bytes starting at off. A range past the end
	// returns what exists (HTTP semantics); one starting past the end is an error.
	GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)

	// Stat reports size and modification time without transferring the object.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// Delete removes the object. A missing key returns ErrNotFound.
	Delete(ctx context.Context, key string) error

	// List returns the keys under prefix, sorted. For GC and index rebuilds,
	// not for API listings.
	List(ctx context.Context, prefix string) ([]string, error)

	// Name identifies the backend in logs.
	Name() string
}

// ValidateKey reports whether key is a legal artifact key. This is a security
// boundary: on the fs backend a key becomes a path, and build IDs can derive
// from a caller-supplied name.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	case len(key) > 512:
		return fmt.Errorf("%w: longer than 512 bytes", ErrInvalidKey)
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("%w: %q is absolute", ErrInvalidKey, key)
	case strings.Contains(key, `\`):
		return fmt.Errorf("%w: %q contains a backslash", ErrInvalidKey, key)
	case strings.Contains(key, "\x00"):
		return fmt.Errorf("%w: contains a NUL byte", ErrInvalidKey)
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: %q has an empty path segment", ErrInvalidKey, key)
		case ".", "..":
			return fmt.Errorf("%w: %q contains a relative segment", ErrInvalidKey, key)
		}
		// A leading dot would hide a key from a directory listing on the fs backend.
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("%w: %q has a segment starting with a dot", ErrInvalidKey, key)
		}
	}
	return nil
}
