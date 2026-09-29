package header

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func testHeader(t *testing.T) *Header {
	t.Helper()
	h, err := New("bld-1", 4096, 4096*10, sha256.Sum256([]byte("payload")), 1788000000)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	b, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.BuildID != h.BuildID || got.BlockSize != h.BlockSize || got.TotalSize != h.TotalSize {
		t.Fatalf("round trip differs: %+v", got)
	}
	if got.CreatedUnix != h.CreatedUnix {
		t.Errorf("CreatedUnix = %d, want %d", got.CreatedUnix, h.CreatedUnix)
	}
	if len(got.Mapping) != 1 || got.Mapping[0].BlockCount != 10 {
		t.Fatalf("mapping = %+v", got.Mapping)
	}
	if sum, ok := got.SelfChecksum(); !ok || sum != sha256.Sum256([]byte("payload")) {
		t.Error("checksum did not survive the round trip")
	}
}

func TestReadV1WithV2Reader(t *testing.T) {
	t.Parallel()

	h := testHeader(t)
	v1Bytes, err := h.marshalVersion(v1)
	if err != nil {
		t.Fatalf("write v1: %v", err)
	}

	got, err := Unmarshal(v1Bytes)
	if err != nil {
		t.Fatalf("the current reader cannot read a v1 header: %v", err)
	}
	if got.FormatVersion != v1 {
		t.Errorf("FormatVersion = %d, want 1 — a reader must not silently claim a header is newer than it is", got.FormatVersion)
	}
	if got.CreatedUnix != 0 {
		t.Errorf("CreatedUnix = %d, want 0: v1 does not carry it", got.CreatedUnix)
	}
	if got.BuildID != h.BuildID || got.TotalSize != h.TotalSize || len(got.Mapping) != len(h.Mapping) {
		t.Fatalf("v1 fields did not survive: %+v", got)
	}
}

func TestV1AndV2DifferOnlyByTheAddedField(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	a, err := h.marshalVersion(v1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.marshalVersion(v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(b)-len(a) != 8 {
		t.Fatalf("v2 is %d bytes larger than v1, want exactly 8 — new fields must be APPENDED", len(b)-len(a))
	}
}

func TestUnknownVersionIsNamedNotPartiallyParsed(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	b, _ := h.Marshal()
	b[4], b[5] = 0xFF, 0x00

	_, err := Unmarshal(b)
	if !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("err = %v, want ErrUnknownVersion", err)
	}
}

func TestBadMagicIsRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"too short", []byte{'P', 'D'}},
		{"wrong magic", append([]byte("XXXX"), 1, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Unmarshal(tc.in); !errors.Is(err, ErrBadMagic) {
				t.Fatalf("err = %v, want ErrBadMagic", err)
			}
		})
	}
}

func TestTruncationAtEveryOffset(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	full, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for n := range len(full) {
		got, err := Unmarshal(full[:n])
		if err == nil {
			t.Fatalf("truncating to %d bytes still parsed: %+v", n, got)
		}
	}
}

func TestAbsurdEntryCountIsRefusedBeforeAllocating(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	b, _ := h.Marshal()
	// The entry count is the last 4-byte field before the entries themselves.
	idx := len(b) - EntrySize - 4
	b[idx], b[idx+1], b[idx+2], b[idx+3] = 0xFF, 0xFF, 0xFF, 0x7F

	_, err := Unmarshal(b)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestValidateRejectsBrokenMappings(t *testing.T) {
	t.Parallel()

	base := func() *Header { return testHeader(t) }
	cases := []struct {
		name   string
		mutate func(*Header)
	}{
		{"gap in the mapping", func(h *Header) {
			h.Mapping = []Entry{{StartBlock: 0, BlockCount: 4}, {StartBlock: 6, BlockCount: 4}}
		}},
		{"overlapping entries", func(h *Header) {
			h.Mapping = []Entry{{StartBlock: 0, BlockCount: 6}, {StartBlock: 4, BlockCount: 6}}
		}},
		{"short coverage", func(h *Header) {
			h.Mapping = []Entry{{StartBlock: 0, BlockCount: 4}}
		}},
		{"past the end", func(h *Header) {
			h.Mapping = []Entry{{StartBlock: 0, BlockCount: 99}}
		}},
		{"zero-length entry", func(h *Header) {
			h.Mapping = []Entry{{StartBlock: 0, BlockCount: 0}, {StartBlock: 0, BlockCount: 10}}
		}},
		{"build index out of range", func(h *Header) {
			h.Mapping = []Entry{{StartBlock: 0, BlockCount: 10, BuildIndex: 7}}
		}},
		{"build in the table with no data record", func(h *Header) {
			h.BuildIDs = append(h.BuildIDs, "ghost")
		}},
		{"zero block size", func(h *Header) { h.BlockSize = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := base()
			tc.mutate(h)
			if err := h.Validate(); err == nil {
				t.Fatal("Validate accepted a broken header")
			}
			if _, err := h.Marshal(); err == nil {
				t.Error("Marshal accepted a broken header")
			}
		})
	}
}

// A size past int64 would turn into a negative file or range offset in every
// reader; it is refused before the mapping is looked at.
func TestValidateRejectsSizePastInt64(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	h.TotalSize = 1 << 63
	if err := h.Validate(); err == nil || !strings.Contains(err.Error(), "int64") {
		t.Fatalf("Validate = %v, want the int64 refusal", err)
	}
}

func TestLocate(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	// Blocks 0-4 from the parent, 5-9 from this build: the shape of a paused sandbox.
	h.BuildIDs = []string{"bld-1", "parent"}
	h.Builds["parent"] = BuildData{Size: 4096 * 5}
	h.Mapping = []Entry{
		{StartBlock: 0, BlockCount: 5, BuildIndex: 1, SourceStartBlock: 2},
		{StartBlock: 5, BlockCount: 5, BuildIndex: 0, SourceStartBlock: 0},
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("a diffed header should be valid: %v", err)
	}

	id, src, err := h.Locate(1)
	if err != nil || id != "parent" || src != 3 {
		t.Fatalf("Locate(1) = %q,%d,%v; want parent,3,nil", id, src, err)
	}
	id, src, err = h.Locate(5)
	if err != nil || id != "bld-1" || src != 0 {
		t.Fatalf("Locate(5) = %q,%d,%v; want bld-1,0,nil", id, src, err)
	}
	if _, _, err := h.Locate(10); err == nil {
		t.Fatal("Locate past the end must fail rather than return a plausible block")
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := New("", 4096, 10, [32]byte{}, 0); err == nil {
		t.Error("want an error for an empty build id")
	}
	if _, err := New("b", 0, 10, [32]byte{}, 0); err == nil {
		t.Error("want an error for a zero block size")
	}
}

func TestEmptyArtifact(t *testing.T) {
	t.Parallel()
	h, err := New("bld-empty", 4096, 0, [32]byte{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mapping) != 0 || got.Blocks() != 0 {
		t.Fatalf("empty artifact should have no blocks, got %+v", got.Mapping)
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	t.Parallel()
	h := testHeader(t)
	a, _ := h.Marshal()
	b, _ := h.Marshal()
	if !bytes.Equal(a, b) {
		t.Fatal("encoding must be deterministic; a changing header defeats content comparison and caching")
	}
}
