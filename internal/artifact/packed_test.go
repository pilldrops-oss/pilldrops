package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
)

func packedStore(t *testing.T) StorageProvider {
	t.Helper()
	s, err := Open(KindFS, Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func roundTrip(t *testing.T, s StorageProvider, h *header.Header) []byte {
	t.Helper()
	raw, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := header.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	keyOf := func(id string) (string, error) { return Key(id, NameMemfile) }
	if err := GetPacked(context.Background(), s, keyOf, back, f); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f.Name())
	return got
}

// Only data blocks are stored; the artifact comes back byte for byte, zero
// runs and a short last block included, through an encoded header.
func TestPackedStoresOnlyDataBlocks(t *testing.T) {
	const bs = 4096
	var want []byte
	want = append(want, make([]byte, 3*bs)...)                // leading zeros
	want = append(want, bytes.Repeat([]byte("a"), 2*bs)...)   // data
	want = append(want, make([]byte, 5*bs)...)                // a hole
	want = append(want, bytes.Repeat([]byte("b"), bs+100)...) // data, then
	want = append(want, make([]byte, 700)...)                 // a short zero-tailed last block
	s := packedStore(t)
	key, _ := Key("bld-1", NameMemfile)
	h, err := PutPacked(context.Background(), s, key, "bld-1", bs, bytes.NewReader(want), 1)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(3*bs+100+700) {
		t.Errorf("stored %d bytes, want only the data blocks (%d)", info.Size, 3*bs+800)
	}
	if h.Whole() {
		t.Error("a packed artifact reads as whole: a v2 reader would serve the packed bytes")
	}
	if got := roundTrip(t, s, h); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes back, want %d identical", len(got), len(want))
	}
}

// An artifact of zeros stores nothing and still comes back full length.
func TestPackedAllZero(t *testing.T) {
	s := packedStore(t)
	key, _ := Key("bld-z", NameMemfile)
	want := make([]byte, 10*4096+7)
	h, err := PutPacked(context.Background(), s, key, "bld-z", 4096, bytes.NewReader(want), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, s, h); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d zeros", len(got), len(want))
	}
}

// Tampered stored data is refused, not served.
func TestPackedRefusesTamperedData(t *testing.T) {
	s := packedStore(t)
	key, _ := Key("bld-t", NameMemfile)
	h, err := PutPacked(context.Background(), s, key, "bld-t", 4096, bytes.NewReader(bytes.Repeat([]byte("x"), 8192)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), key, bytes.NewReader(bytes.Repeat([]byte("y"), 8192)), 8192); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Create(filepath.Join(t.TempDir(), "out"))
	defer f.Close()
	keyOf := func(id string) (string, error) { return Key(id, NameMemfile) }
	if err := GetPacked(context.Background(), s, keyOf, h, f); err == nil {
		t.Fatal("tampered data was accepted")
	}
}

// An artifact mapped entirely onto another build reads that build's data, as a
// deduplicated root image does.
func TestPackedReadsAnotherBuild(t *testing.T) {
	s := packedStore(t)
	data := bytes.Repeat([]byte("rootfs"), 3000)
	sharedKey, _ := Key("rfs-shared", NameMemfile)
	shared, err := PutPacked(context.Background(), s, sharedKey, "rfs-shared", 4096, bytes.NewReader(data), 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := &header.Header{
		BuildID: "bld-ref", BaseBuildID: "rfs-shared", BlockSize: 4096, TotalSize: shared.TotalSize,
		BuildIDs: []string{"bld-ref", "rfs-shared"},
		Builds:   map[string]header.BuildData{"bld-ref": {}, "rfs-shared": shared.Builds["rfs-shared"]},
		Mapping:  []header.Entry{{StartBlock: 0, BlockCount: uint32(shared.Blocks()), BuildIndex: 1}},
	}
	if got := roundTrip(t, s, ref); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the shared build's %d", len(got), len(data))
	}
}

// Every logical block reads back alone, by range, verified; zero blocks are
// reported as such; a tampered stored block is refused.
func TestReadPackedBlock(t *testing.T) {
	const bs = 4096
	var want []byte
	want = append(want, make([]byte, 2*bs)...)
	want = append(want, bytes.Repeat([]byte("m"), 3*bs)...)
	want = append(want, make([]byte, bs)...)
	want = append(want, bytes.Repeat([]byte("t"), 500)...) // short last block
	s := packedStore(t)
	key, _ := Key("bld-r", NameMemfile)
	h, sums, err := PutPackedHashed(context.Background(), s, key, "bld-r", bs, bytes.NewReader(want), 1)
	if err != nil {
		t.Fatal(err)
	}
	for b := uint64(0); b < h.Blocks(); b++ {
		p := bytes.Repeat([]byte{0xff}, bs)
		zero, err := ReadPackedBlock(context.Background(), s, key, h, sums, b, p)
		if err != nil {
			t.Fatalf("block %d: %v", b, err)
		}
		end := min((b+1)*bs, uint64(len(want)))
		wantBlock := make([]byte, bs)
		copy(wantBlock, want[b*bs:end])
		if zero {
			if !bytes.Equal(wantBlock, make([]byte, bs)) {
				t.Fatalf("block %d reported zero but holds data", b)
			}
			continue
		}
		if !bytes.Equal(p, wantBlock) {
			t.Fatalf("block %d read back wrong", b)
		}
	}
	// Overwrite the stored data with the same length of other bytes.
	info, _ := s.Stat(context.Background(), key)
	if err := s.Put(context.Background(), key, bytes.NewReader(bytes.Repeat([]byte("x"), int(info.Size))), info.Size); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPackedBlock(context.Background(), s, key, h, sums, 2, make([]byte, bs)); err == nil {
		t.Fatal("a tampered block was served")
	}
}
