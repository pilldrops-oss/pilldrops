package header

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// Wire format. Little-endian throughout.
//
//	magic          4     "PDAH"
//	version        2     read before anything else
//	blockSize      4
//	totalSize      8
//	createdUnix    8     v2 only
//	buildID        2+n   length-prefixed
//	baseBuildID    2+n
//	buildCount     2
//	  per build:   2+n   id
//	               8     size
//	               32    sha256
//	entryCount     4
//	  per entry:   14    see Entry
//
// The version is read first and selects the layout. A published version's
// layout is frozen; a newer reader parses old bytes in the old order and
// defaults what that version did not carry.

const (
	v1 uint16 = 1
	// v2 adds CreatedUnix.
	v2 uint16 = 2
	// v3 has v2's layout and says the mapping is authoritative: the stored
	// object is only the blocks this build holds, packed, and a reader must
	// assemble the artifact from the mapping. A v2 reader would verify the
	// packed bytes and serve them as the artifact, so v3 exists to be refused.
	v3 uint16 = 3

	maxStringLen = 1 << 12
	maxBuilds    = 1<<16 - 1
	// maxEntries bounds allocation from a hostile or corrupt header.
	maxEntries = 8 << 20
)

// Marshal encodes the header at this build's format version.
func (h *Header) Marshal() ([]byte, error) {
	if h.Whole() {
		return h.marshalVersion(v2) // readable by every node that predates packing
	}
	return h.marshalVersion(v3)
}

// marshalVersion encodes at a specific version; only tests write old ones.
func (h *Header) marshalVersion(ver uint16) ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	if ver != v1 && ver != v2 && ver != v3 {
		return nil, fmt.Errorf("%w: cannot write version %d", ErrUnknownVersion, ver)
	}

	var b bytes.Buffer
	b.Write(Magic[:])
	put16(&b, ver)
	put32(&b, h.BlockSize)
	put64(&b, h.TotalSize)
	if ver >= v2 {
		put64(&b, uint64(h.CreatedUnix))
	}
	if err := putStr(&b, h.BuildID); err != nil {
		return nil, err
	}
	if err := putStr(&b, h.BaseBuildID); err != nil {
		return nil, err
	}

	if len(h.BuildIDs) > maxBuilds {
		return nil, fmt.Errorf("%w: %d builds exceeds the ceiling", ErrMalformed, len(h.BuildIDs))
	}
	put16(&b, uint16(len(h.BuildIDs)))
	for _, id := range h.BuildIDs {
		if err := putStr(&b, id); err != nil {
			return nil, err
		}
		bd := h.Builds[id]
		put64(&b, bd.Size)
		b.Write(bd.SHA256[:])
	}

	if len(h.Mapping) > maxEntries {
		return nil, fmt.Errorf("%w: %d mapping entries exceeds the ceiling", ErrMalformed, len(h.Mapping))
	}
	put32(&b, uint32(len(h.Mapping)))
	for _, e := range h.Mapping {
		put32(&b, e.StartBlock)
		put32(&b, e.BlockCount)
		put16(&b, e.BuildIndex)
		put32(&b, e.SourceStartBlock)
	}
	return b.Bytes(), nil
}

// Unmarshal decodes a header, accepting every version this package has written.
// Every read is bounds-checked and every length capped before allocation,
// since the bytes come from outside the process.
func Unmarshal(data []byte) (*Header, error) {
	r := &reader{b: data}

	magic, err := r.bytes(4)
	if err != nil {
		return nil, fmt.Errorf("%w: too short for a magic number", ErrBadMagic)
	}
	if !bytes.Equal(magic, Magic[:]) {
		return nil, fmt.Errorf("%w: got %q", ErrBadMagic, magic)
	}

	ver, err := r.u16()
	if err != nil {
		return nil, fmt.Errorf("%w: truncated before the version", ErrMalformed)
	}
	if ver != v1 && ver != v2 && ver != v3 {
		return nil, fmt.Errorf("%w: %d (this build reads 1 to 3) — upgrade the reader rather than "+
			"re-publishing the artifact", ErrUnknownVersion, ver)
	}

	h := &Header{FormatVersion: ver}
	if h.BlockSize, err = r.u32(); err != nil {
		return nil, wrap(err, "block size")
	}
	if h.TotalSize, err = r.u64(); err != nil {
		return nil, wrap(err, "total size")
	}
	if ver >= v2 {
		created, err := r.u64()
		if err != nil {
			return nil, wrap(err, "created timestamp")
		}
		h.CreatedUnix = int64(created)
	}

	if h.BuildID, err = r.str(); err != nil {
		return nil, wrap(err, "build id")
	}
	if h.BaseBuildID, err = r.str(); err != nil {
		return nil, wrap(err, "base build id")
	}

	buildCount, err := r.u16()
	if err != nil {
		return nil, wrap(err, "build count")
	}
	h.BuildIDs = make([]string, 0, buildCount)
	h.Builds = make(map[string]BuildData, buildCount)
	for i := range int(buildCount) {
		id, err := r.str()
		if err != nil {
			return nil, wrap(err, fmt.Sprintf("build %d id", i))
		}
		size, err := r.u64()
		if err != nil {
			return nil, wrap(err, fmt.Sprintf("build %d size", i))
		}
		sum, err := r.bytes(sha256.Size)
		if err != nil {
			return nil, wrap(err, fmt.Sprintf("build %d checksum", i))
		}
		var arr [sha256.Size]byte
		copy(arr[:], sum)
		h.BuildIDs = append(h.BuildIDs, id)
		h.Builds[id] = BuildData{Size: size, SHA256: arr}
	}

	entryCount, err := r.u32()
	if err != nil {
		return nil, wrap(err, "entry count")
	}
	if entryCount > maxEntries {
		return nil, fmt.Errorf("%w: header claims %d mapping entries, above the %d ceiling",
			ErrMalformed, entryCount, maxEntries)
	}
	// Check the bytes are actually there before allocating for the claimed count.
	if need := int(entryCount) * EntrySize; r.remaining() < need {
		return nil, fmt.Errorf("%w: header claims %d entries (%d bytes) but only %d bytes remain",
			ErrMalformed, entryCount, need, r.remaining())
	}
	h.Mapping = make([]Entry, 0, entryCount)
	for i := range int(entryCount) {
		var e Entry
		if e.StartBlock, err = r.u32(); err != nil {
			return nil, wrap(err, fmt.Sprintf("entry %d start", i))
		}
		if e.BlockCount, err = r.u32(); err != nil {
			return nil, wrap(err, fmt.Sprintf("entry %d count", i))
		}
		if e.BuildIndex, err = r.u16(); err != nil {
			return nil, wrap(err, fmt.Sprintf("entry %d build index", i))
		}
		if e.SourceStartBlock, err = r.u32(); err != nil {
			return nil, wrap(err, fmt.Sprintf("entry %d source start", i))
		}
		h.Mapping = append(h.Mapping, e)
	}

	if err := h.Validate(); err != nil {
		return nil, err
	}
	return h, nil
}

func wrap(err error, what string) error {
	return fmt.Errorf("%w: reading %s: %w", ErrMalformed, what, err)
}

// reader is a bounds-checked cursor; a truncated header errors instead of panicking.
type reader struct {
	b []byte
	i int
}

func (r *reader) remaining() int { return len(r.b) - r.i }

func (r *reader) bytes(n int) ([]byte, error) {
	if n < 0 || r.remaining() < n {
		return nil, io.ErrUnexpectedEOF
	}
	out := r.b[r.i : r.i+n]
	r.i += n
	return out, nil
}

func (r *reader) u16() (uint16, error) {
	b, err := r.bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (r *reader) u32() (uint32, error) {
	b, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *reader) u64() (uint64, error) {
	b, err := r.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (r *reader) str() (string, error) {
	n, err := r.u16()
	if err != nil {
		return "", err
	}
	if int(n) > maxStringLen {
		return "", fmt.Errorf("string of %d bytes exceeds the %d cap", n, maxStringLen)
	}
	b, err := r.bytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func put16(b *bytes.Buffer, v uint16) { _ = binary.Write(b, binary.LittleEndian, v) }
func put32(b *bytes.Buffer, v uint32) { _ = binary.Write(b, binary.LittleEndian, v) }
func put64(b *bytes.Buffer, v uint64) { _ = binary.Write(b, binary.LittleEndian, v) }

func putStr(b *bytes.Buffer, s string) error {
	if len(s) > maxStringLen {
		return fmt.Errorf("%w: string of %d bytes exceeds the %d cap", ErrMalformed, len(s), maxStringLen)
	}
	put16(b, uint16(len(s)))
	b.WriteString(s)
	return nil
}
