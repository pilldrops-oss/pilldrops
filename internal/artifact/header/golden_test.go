package header

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// goldenV1 is a frozen v1 header. A published version's layout never changes,
// and round-trip tests cannot catch a reader that drifts from old bytes. If
// this fails, fix the reader; do not regenerate the fixture.
const goldenV1 = "5044414801000010000000300000000000000a00626c642d676f6c64656e0a00626c642d676f6c64656e" +
	"01000a00626c642d676f6c64656e0030000000000000dd56de4137951d9c92681b03416ec15f886b4482a2" +
	"7e3a517d32f085244cbe5d010000000000000003000000000000000000"

func TestGoldenV1StillParses(t *testing.T) {
	t.Parallel()

	raw, err := hex.DecodeString(goldenV1)
	if err != nil {
		t.Fatalf("fixture is not valid hex: %v", err)
	}

	h, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("the current reader can no longer parse a v1 header written before it: %v\n"+
			"This means the wire format changed incompatibly. Every artifact already published is\n"+
			"in the old format and cannot be re-encoded — fix the reader, do not regenerate the fixture.", err)
	}

	if h.FormatVersion != v1 {
		t.Errorf("FormatVersion = %d, want 1", h.FormatVersion)
	}
	if h.BuildID != "bld-golden" || h.BaseBuildID != "bld-golden" {
		t.Errorf("build ids = %q/%q", h.BuildID, h.BaseBuildID)
	}
	if h.BlockSize != 4096 {
		t.Errorf("BlockSize = %d, want 4096", h.BlockSize)
	}
	if h.TotalSize != 4096*3 {
		t.Errorf("TotalSize = %d, want %d", h.TotalSize, 4096*3)
	}
	if want := sha256.Sum256([]byte("golden")); h.Builds["bld-golden"].SHA256 != want {
		t.Error("checksum did not survive")
	}
	if len(h.Mapping) != 1 || h.Mapping[0].BlockCount != 3 || h.Mapping[0].StartBlock != 0 {
		t.Errorf("mapping = %+v", h.Mapping)
	}
	// v1 never carried CreatedUnix; it must decode as zero.
	if h.CreatedUnix != 0 {
		t.Errorf("CreatedUnix = %d, want 0 — a v1 header does not carry it, and a non-zero value here "+
			"means the reader is interpreting v1 bytes as if they were v2", h.CreatedUnix)
	}
}
