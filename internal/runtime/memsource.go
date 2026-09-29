package runtime

import "context"

// MemSource supplies a memory image by offset, for a VM restored with its
// memory served on demand rather than read from a file.
type MemSource interface {
	// ReadBlock fills p with the image's bytes at off, or reports zero=true for
	// a block that holds none, leaving p untouched.
	ReadBlock(ctx context.Context, off int64, p []byte) (zero bool, err error)
}

// LazyMemSource opens the source a capture's mem.lazy note names. The node
// sets it at boot with the object store the note refers to; the Firecracker
// engine calls it. Nil: a capture without its memory file cannot boot here.
var LazyMemSource func(ctx context.Context, note []byte) (MemSource, error)
