// Package header is the block map describing which build holds each block of
// an artifact. Today every artifact is one entry from its own build; the map
// exists so a paused sandbox can later be stored as a diff against its template.
package header

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
)

// Version is the newest format this build writes: 3 for a packed or chained
// artifact, 2 for a whole one (Marshal). Readers accept every version ever written.
const Version uint16 = 3

// ZeroBuild is the reserved build a run of all-zero blocks maps to: nothing is
// stored for it and a reader leaves a hole. Its data record is empty.
const ZeroBuild = "zero"

// Magic identifies an artifact header ("PillDrops Artifact Header").
var Magic = [4]byte{'P', 'D', 'A', 'H'}

var (
	// ErrBadMagic means this is not an artifact header.
	ErrBadMagic = errors.New("header: not an artifact header")
	// ErrUnknownVersion means the header was written by a newer build; upgrade
	// the reader rather than re-publishing the artifact.
	ErrUnknownVersion = errors.New("header: unknown format version")
	// ErrMalformed means the bytes are structurally invalid.
	ErrMalformed = errors.New("header: malformed")
)

// BuildData is what is known about one build's data.
type BuildData struct {
	// Size is the uncompressed length of that build's contribution.
	Size uint64
	// SHA256 is over the uncompressed bytes.
	SHA256 [sha256.Size]byte
}

// Entry maps a run of blocks in the logical artifact to a run of blocks in
// some build's stored data. Block indices and a build index keep it at 14
// bytes; the ceilings are 2^32 blocks and 65535 builds per chain.
type Entry struct {
	// StartBlock is where this run begins in the logical artifact.
	StartBlock uint32
	// BlockCount is how many blocks it covers.
	BlockCount uint32
	// BuildIndex selects the build in Header.Builds holding the data.
	BuildIndex uint16
	// SourceStartBlock is where the run begins within that build's data.
	SourceStartBlock uint32
}

// EntrySize is the encoded width of one Entry: 4 + 4 + 2 + 4.
const EntrySize = 14

// Header describes one artifact.
type Header struct {
	// FormatVersion is the version this header was read from, or 0 for one built in memory.
	FormatVersion uint16

	// BuildID is the build this artifact belongs to.
	BuildID string
	// BaseBuildID is the root of the chain; equal to BuildID for an artifact that is its own base.
	BaseBuildID string

	// BlockSize is the granularity of the mapping, in bytes.
	BlockSize uint32
	// TotalSize is the logical length of the artifact.
	TotalSize uint64

	// BuildIDs is the table Entry.BuildIndex selects from. Index 0 is this artifact's own build.
	BuildIDs []string
	// Builds carries size and checksum per build ID.
	Builds map[string]BuildData

	// Mapping is ordered by StartBlock and covers the artifact without gaps or overlaps.
	Mapping []Entry

	// CreatedUnix was added in v2; a v1 header decodes with it zero.
	CreatedUnix int64
}

// New builds the header for a whole, undiffed artifact: one entry covering
// everything from its own build.
func New(buildID string, blockSize uint32, totalSize uint64, sum [sha256.Size]byte, createdUnix int64) (*Header, error) {
	if buildID == "" {
		return nil, fmt.Errorf("%w: build id is required", ErrMalformed)
	}
	if blockSize == 0 {
		return nil, fmt.Errorf("%w: block size must be non-zero", ErrMalformed)
	}
	blocks := blocksFor(totalSize, blockSize)
	if blocks > 1<<32-1 {
		return nil, fmt.Errorf("%w: %d blocks exceeds the 2^32 ceiling at block size %d",
			ErrMalformed, blocks, blockSize)
	}
	h := &Header{
		FormatVersion: Version,
		BuildID:       buildID,
		BaseBuildID:   buildID,
		BlockSize:     blockSize,
		TotalSize:     totalSize,
		BuildIDs:      []string{buildID},
		Builds:        map[string]BuildData{buildID: {Size: totalSize, SHA256: sum}},
		CreatedUnix:   createdUnix,
	}
	if blocks > 0 {
		h.Mapping = []Entry{{StartBlock: 0, BlockCount: uint32(blocks), BuildIndex: 0, SourceStartBlock: 0}}
	}
	return h, nil
}

// blocksFor is the block count for a byte length, rounding up.
func blocksFor(size uint64, blockSize uint32) uint64 {
	if blockSize == 0 {
		return 0
	}
	return (size + uint64(blockSize) - 1) / uint64(blockSize)
}

// Whole reports an artifact stored entire in its own build: one entry from
// build 0 at block 0, readable by any reader, packed or not.
func (h *Header) Whole() bool {
	return len(h.BuildIDs) == 1 && len(h.Mapping) <= 1 &&
		(len(h.Mapping) == 0 || (h.Mapping[0].BuildIndex == 0 && h.Mapping[0].SourceStartBlock == 0))
}

// Blocks reports how many blocks the artifact spans.
func (h *Header) Blocks() uint64 { return blocksFor(h.TotalSize, h.BlockSize) }

// SelfChecksum returns the checksum of this artifact's own build.
func (h *Header) SelfChecksum() ([sha256.Size]byte, bool) {
	bd, ok := h.Builds[h.BuildID]
	return bd.SHA256, ok
}

// Validate checks the invariants a reader depends on: build indices resolve
// and the mapping is ordered, gapless, and covers exactly the artifact.
func (h *Header) Validate() error {
	switch {
	case h.BuildID == "":
		return fmt.Errorf("%w: empty build id", ErrMalformed)
	case h.BlockSize == 0:
		return fmt.Errorf("%w: zero block size", ErrMalformed)
	case h.TotalSize > math.MaxInt64:
		// Readers turn sizes and offsets into int64 file and range offsets.
		return fmt.Errorf("%w: total size %d does not fit an int64", ErrMalformed, h.TotalSize)
	case len(h.BuildIDs) == 0:
		return fmt.Errorf("%w: empty build table", ErrMalformed)
	case len(h.BuildIDs) > 1<<16-1:
		return fmt.Errorf("%w: build table of %d exceeds the 2^16 ceiling", ErrMalformed, len(h.BuildIDs))
	}
	for i, id := range h.BuildIDs {
		if id == "" {
			return fmt.Errorf("%w: build table entry %d is empty", ErrMalformed, i)
		}
		if _, ok := h.Builds[id]; !ok {
			return fmt.Errorf("%w: build %q is in the table but has no data record", ErrMalformed, id)
		}
	}

	want := h.Blocks()
	var covered uint64
	for i, e := range h.Mapping {
		if e.BlockCount == 0 {
			return fmt.Errorf("%w: mapping entry %d covers zero blocks", ErrMalformed, i)
		}
		if int(e.BuildIndex) >= len(h.BuildIDs) {
			return fmt.Errorf("%w: mapping entry %d references build index %d of %d",
				ErrMalformed, i, e.BuildIndex, len(h.BuildIDs))
		}
		if uint64(e.StartBlock) != covered {
			return fmt.Errorf("%w: mapping entry %d starts at block %d, expected %d — "+
				"the mapping must be ordered and gapless", ErrMalformed, i, e.StartBlock, covered)
		}
		next := uint64(e.StartBlock) + uint64(e.BlockCount)
		if next > want {
			return fmt.Errorf("%w: mapping entry %d ends at block %d, past the artifact's %d",
				ErrMalformed, i, next, want)
		}
		covered = next
	}
	if covered != want {
		return fmt.Errorf("%w: mapping covers %d blocks, artifact spans %d", ErrMalformed, covered, want)
	}
	return nil
}

// Locate resolves a logical block to the build holding it and the block index
// within that build's data.
func (h *Header) Locate(block uint64) (buildID string, sourceBlock uint64, err error) {
	// ponytail: linear scan, binary search once diff chains have many entries.
	for _, e := range h.Mapping {
		start := uint64(e.StartBlock)
		if block >= start && block < start+uint64(e.BlockCount) {
			return h.BuildIDs[e.BuildIndex], uint64(e.SourceStartBlock) + (block - start), nil
		}
	}
	return "", 0, fmt.Errorf("%w: block %d is not covered by the mapping", ErrMalformed, block)
}
