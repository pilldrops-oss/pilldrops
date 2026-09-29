package artifact

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Kind names a backend. Adding one is a new file implementing StorageProvider,
// a case in Open, a line in Kinds, and a run of the conformance suite.
type Kind string

const (
	// KindFS stores artifacts in a directory. The default.
	KindFS Kind = "fs"
	// KindControlPlane fetches from the control plane with the node's existing credential.
	KindControlPlane Kind = "controlplane"
	// KindS3 is any S3-protocol object store: AWS S3, Cloudflare R2, MinIO.
	KindS3 Kind = "s3"
)

// Kinds returns the backends this build can open.
func Kinds() []string {
	ks := []string{string(KindFS), string(KindControlPlane), string(KindS3)}
	sort.Strings(ks)
	return ks
}

// Options is everything any backend might need; each ignores what does not apply.
type Options struct {
	// Root is the directory for KindFS.
	Root string
	// BaseURL and Credential are for KindControlPlane.
	BaseURL    string
	Credential string
	// HTTPClient is optional; nil gets a sensible default.
	HTTPClient *http.Client
	// S3 is for KindS3.
	S3 S3Options
}

// Open constructs the named backend. Unimplemented kinds are refused, never
// silently replaced with fs.
func Open(kind Kind, opts Options) (StorageProvider, error) {
	switch kind {
	case KindFS, "":
		return NewFS(opts.Root)
	case KindControlPlane:
		return NewControlPlane(opts.BaseURL, opts.Credential, opts.HTTPClient)
	case KindS3:
		o := opts.S3
		if o.HTTPClient == nil {
			o.HTTPClient = opts.HTTPClient
		}
		return NewS3(o)
	default:
		return nil, fmt.Errorf("artifact: unknown store %q (available: %s)",
			kind, strings.Join(Kinds(), ", "))
	}
}

// Shared reports whether two nodes pointed at a backend see the same objects.
// FS is false even on NFS, which lacks the atomicity Put relies on.
func Shared(kind Kind) bool {
	switch kind {
	case KindControlPlane, KindS3:
		return true
	default:
		return false
	}
}

// Copy copies every object under prefix from src to dst, verifying each by size.
func Copy(ctx context.Context, src, dst StorageProvider, prefix string) (copied int, err error) {
	keys, err := src.List(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("artifact: list source: %w", err)
	}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		info, err := src.Stat(ctx, key)
		if err != nil {
			return copied, fmt.Errorf("artifact: stat %q on source: %w", key, err)
		}
		rc, err := src.Get(ctx, key)
		if err != nil {
			return copied, fmt.Errorf("artifact: read %q from source: %w", key, err)
		}
		err = dst.Put(ctx, key, rc, info.Size)
		rc.Close()
		if err != nil {
			return copied, fmt.Errorf("artifact: write %q to destination: %w", key, err)
		}
		got, err := dst.Stat(ctx, key)
		if err != nil {
			return copied, fmt.Errorf("artifact: verify %q on destination: %w", key, err)
		}
		if got.Size != info.Size {
			return copied, fmt.Errorf("artifact: %q copied short: %d bytes at source, %d at destination",
				key, info.Size, got.Size)
		}
		copied++
	}
	return copied, nil
}
