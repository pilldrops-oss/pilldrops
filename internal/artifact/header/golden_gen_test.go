package header

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Run with -run TestPrintGoldenV1 -v to print the fixture for golden_test.go.
// Regenerating it is a reviewed decision, never automatic.
func TestPrintGoldenV1(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	h, err := New("bld-golden", 4096, 4096*3, sha256.Sum256([]byte("golden")), 1788000000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.marshalVersion(v1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("goldenV1 = %q", hex.EncodeToString(b))
}
