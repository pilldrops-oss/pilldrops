package artifact

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Zero chunks become holes, and the file still reads back byte for byte,
// trailing zeros included once the length is set.
func TestSparseWriterRoundTrips(t *testing.T) {
	want := append(append(make([]byte, 100<<10), []byte("data")...), make([]byte, 70<<10)...)
	f, err := os.Create(filepath.Join(t.TempDir(), "img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := io.CopyBuffer(&SparseWriter{F: f}, bytes.NewReader(want), make([]byte, 32<<10))
	if err != nil || f.Truncate(n) != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, want) {
		t.Fatalf("read back %d bytes, want %d identical", len(got), len(want))
	}
}
