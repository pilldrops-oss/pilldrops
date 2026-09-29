package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
)

// Packed objects. A memory image or a disk overlay is mostly zero blocks, and
// storing them whole moved gigabytes of zeros per pause and per move. PutPacked
// stores only the blocks that hold data, back to back, and returns the header
// whose mapping says where each logical block lives: a run of this build's
// packed blocks, or the zero build, which is a hole. GetPacked rebuilds the
// artifact from any mapping, including one that points into other builds, as a
// deduplicated root image does.

// Putter is the one store method PutPacked needs.
type Putter interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
}

// Getter is the one store method GetPacked needs.
type Getter interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// PutPacked stores the non-zero blocks of r under key as buildID's data and
// returns the header describing the whole artifact.
func PutPacked(ctx context.Context, store Putter, key, buildID string, blockSize uint32, r io.Reader, now int64) (*header.Header, error) {
	h, _, err := PutPackedHashed(ctx, store, key, buildID, blockSize, r, now)
	return h, err
}

// PutPackedHashed is PutPacked also returning the SHA-256 of every stored
// block, in stored order: what lets a reader fetch one block by range and
// verify it alone, as lazy memory does, where the build checksum covers only
// the whole object.
func PutPackedHashed(ctx context.Context, store Putter, key, buildID string, blockSize uint32, r io.Reader, now int64) (*header.Header, [][sha256.Size]byte, error) {
	var blockSums [][sha256.Size]byte
	if blockSize == 0 {
		return nil, nil, errors.New("artifact: packed block size must be non-zero")
	}
	pr, pw := io.Pipe()
	put := make(chan error, 1)
	go func() {
		err := store.Put(ctx, key, pr, -1)
		_ = pr.CloseWithError(err)
		put <- err
	}()

	h := &header.Header{
		BuildID: buildID, BaseBuildID: buildID, BlockSize: blockSize,
		BuildIDs: []string{buildID}, CreatedUnix: now,
	}
	zeroIndex := -1
	sum := sha256.New()
	var total, packedBlocks, packedBytes uint64
	buf := make([]byte, blockSize)
	zero := make([]byte, blockSize)
	var readErr error
	for block := uint32(0); ; block++ {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			total += uint64(n)
			if bytes.Equal(buf[:n], zero[:n]) {
				if zeroIndex < 0 {
					zeroIndex = len(h.BuildIDs)
					h.BuildIDs = append(h.BuildIDs, header.ZeroBuild)
				}
				appendRun(h, block, uint16(zeroIndex), 0)
			} else {
				if _, werr := pw.Write(buf[:n]); werr != nil {
					readErr = werr
					break
				}
				sum.Write(buf[:n])
				blockSums = append(blockSums, sha256.Sum256(buf[:n]))
				appendRun(h, block, 0, uint32(packedBlocks))
				packedBlocks++
				packedBytes += uint64(n)
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	_ = pw.CloseWithError(readErr)
	if perr := <-put; perr != nil {
		return nil, nil, perr
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("artifact: read %q: %w", key, readErr)
	}
	var digest [sha256.Size]byte
	copy(digest[:], sum.Sum(nil))
	h.TotalSize = total
	h.Builds = map[string]header.BuildData{buildID: {Size: packedBytes, SHA256: digest}}
	if zeroIndex >= 0 {
		h.Builds[header.ZeroBuild] = header.BuildData{}
	}
	if err := h.Validate(); err != nil {
		return nil, nil, err
	}
	return h, blockSums, nil
}

// appendRun maps logical block to (build, source), extending the last entry
// when it continues the same run.
func appendRun(h *header.Header, block uint32, build uint16, source uint32) {
	if n := len(h.Mapping); n > 0 {
		last := &h.Mapping[n-1]
		contiguous := build == last.BuildIndex && h.BuildIDs[build] == header.ZeroBuild ||
			build == last.BuildIndex && source == last.SourceStartBlock+last.BlockCount
		if contiguous && last.StartBlock+last.BlockCount == block {
			last.BlockCount++
			return
		}
	}
	h.Mapping = append(h.Mapping, header.Entry{StartBlock: block, BlockCount: 1, BuildIndex: build, SourceStartBlock: source})
}

// GetPacked writes the artifact hdr describes into f, zero runs as holes, and
// verifies every build it reads against the checksum the header records for
// it. keyOf names a build's stored data. Each build's data is read once, in
// order, so its checksum covers exactly what was stored.
func GetPacked(ctx context.Context, store Getter, keyOf func(buildID string) (string, error), hdr *header.Header, f *os.File) error {
	if err := hdr.Validate(); err != nil {
		return err
	}
	if err := f.Truncate(int64(hdr.TotalSize)); err != nil {
		return err
	}
	bs := uint64(hdr.BlockSize)
	used := make([]bool, len(hdr.BuildIDs))
	for _, e := range hdr.Mapping {
		used[e.BuildIndex] = true
	}
	for bi, id := range hdr.BuildIDs {
		// A build the mapping never points at stores nothing for this
		// artifact: its own, when every block comes from a shared build.
		if id == header.ZeroBuild || !used[bi] {
			continue
		}
		want := hdr.Builds[id]
		key, err := keyOf(id)
		if err != nil {
			return err
		}
		rc, err := store.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("artifact: read build %s of %s: %w", id, hdr.BuildID, err)
		}
		sum := sha256.New()
		src := io.TeeReader(rc, sum)
		var pos uint64 // bytes of this build's data consumed
		for _, e := range hdr.Mapping {
			if int(e.BuildIndex) != bi {
				continue
			}
			from := uint64(e.SourceStartBlock) * bs
			if from < pos {
				_ = rc.Close()
				return fmt.Errorf("%w: build %s is referenced out of order", header.ErrMalformed, id)
			}
			if _, err := io.CopyN(io.Discard, src, int64(from-pos)); err != nil {
				_ = rc.Close()
				return fmt.Errorf("artifact: skip within build %s: %w", id, err)
			}
			n := uint64(e.BlockCount) * bs
			start := uint64(e.StartBlock) * bs
			if start+n > hdr.TotalSize {
				n = hdr.TotalSize - start // the artifact's last block may be short
			}
			copied, err := io.Copy(io.NewOffsetWriter(f, int64(start)), io.LimitReader(src, int64(n)))
			if err == nil && uint64(copied) != n {
				err = fmt.Errorf("stored data ends %d bytes early", n-uint64(copied))
			}
			if err != nil {
				_ = rc.Close()
				return fmt.Errorf("artifact: copy build %s: %w", id, err)
			}
			pos = from + n
		}
		// The rest of the build counts toward its checksum even where this
		// artifact does not use it.
		if _, err := io.Copy(io.Discard, src); err != nil {
			_ = rc.Close()
			return fmt.Errorf("artifact: read build %s: %w", id, err)
		}
		_ = rc.Close()
		if !bytes.Equal(sum.Sum(nil), want.SHA256[:]) {
			return fmt.Errorf("artifact: build %s of %s failed verification: its data does not match "+
				"the checksum its header records", id, hdr.BuildID)
		}
	}
	return nil
}

// Ranger is the store method ReadPackedBlock needs.
type Ranger interface {
	GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)
}

// ReadPackedBlock reads logical block b of a packed artifact stored under key
// into p (at most one block; the rest of p is zeroed), verifying it against
// its stored-order checksum. zero reports a block the artifact never stored,
// leaving p untouched. Only this build's own data is read: a block mapped
// onto another build is refused, since its checksums are not these.
func ReadPackedBlock(ctx context.Context, store Ranger, key string, hdr *header.Header,
	sums [][sha256.Size]byte, b uint64, p []byte) (zero bool, err error) {
	for _, e := range hdr.Mapping {
		start := uint64(e.StartBlock)
		if b < start || b >= start+uint64(e.BlockCount) {
			continue
		}
		id := hdr.BuildIDs[e.BuildIndex]
		if id == header.ZeroBuild {
			return true, nil
		}
		if e.BuildIndex != 0 {
			return false, fmt.Errorf("artifact: block %d of %s lives in build %s, which a block read does not follow", b, hdr.BuildID, id)
		}
		stored := uint64(e.SourceStartBlock) + (b - start)
		if stored >= uint64(len(sums)) {
			return false, fmt.Errorf("artifact: block %d of %s has no checksum", b, hdr.BuildID)
		}
		bs := uint64(hdr.BlockSize)
		n := bs
		if end := b*bs + bs; end > hdr.TotalSize {
			n = hdr.TotalSize - b*bs // the artifact's short last block
		}
		if uint64(len(p)) < n {
			n = uint64(len(p))
		}
		rc, err := store.GetRange(ctx, key, int64(stored*bs), int64(n))
		if err != nil {
			return false, err
		}
		_, err = io.ReadFull(rc, p[:n])
		_ = rc.Close()
		if err != nil {
			return false, fmt.Errorf("artifact: read block %d of %s: %w", b, hdr.BuildID, err)
		}
		if sha256.Sum256(p[:n]) != sums[stored] {
			return false, fmt.Errorf("artifact: block %d of %s failed verification", b, hdr.BuildID)
		}
		clear(p[n:])
		return false, nil
	}
	return false, fmt.Errorf("artifact: block %d is outside %s", b, hdr.BuildID)
}
