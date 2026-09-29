package header

import "testing"

// Malformed header bytes must error, never panic or yield an unresolvable mapping.
func FuzzUnmarshal(f *testing.F) {
	h, err := New("bld-1", 4096, 4096*3, [32]byte{1, 2, 3}, 99)
	if err != nil {
		f.Fatal(err)
	}
	valid, err := h.Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	if v1Bytes, err := h.marshalVersion(v1); err == nil {
		f.Add(v1Bytes)
	}
	f.Add([]byte{})
	f.Add([]byte("PDAH"))
	f.Add(append([]byte("PDAH"), 2, 0))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Unmarshal(data)
		if err != nil {
			return
		}
		if got.BlockSize == 0 {
			t.Fatal("decoded a header with a zero block size")
		}
		for i, e := range got.Mapping {
			if int(e.BuildIndex) >= len(got.BuildIDs) {
				t.Fatalf("entry %d references build index %d of %d", i, e.BuildIndex, len(got.BuildIDs))
			}
		}
		for blk := range got.Blocks() {
			if _, _, err := got.Locate(blk); err != nil {
				t.Fatalf("block %d of %d does not resolve: %v", blk, got.Blocks(), err)
			}
		}
	})
}
