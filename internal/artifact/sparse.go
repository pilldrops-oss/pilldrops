package artifact

import (
	"bytes"
	"io"
	"os"
)

// SparseWriter writes all-zero chunks as holes: a memory image or a disk
// overlay is mostly zero pages, and writing them costs disk time and space for
// nothing a read would not return anyway. The caller sets the final length
// with Truncate, since a trailing hole writes nothing.
type SparseWriter struct{ F *os.File }

var zeroChunk [64 << 10]byte

func (w *SparseWriter) Write(p []byte) (int, error) {
	if len(p) <= len(zeroChunk) && bytes.Equal(p, zeroChunk[:len(p)]) {
		if _, err := w.F.Seek(int64(len(p)), io.SeekCurrent); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return w.F.Write(p)
}
