package nodeboot

import "testing"

func TestStorageCapGuardError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		production bool
		enforced   bool
		known      bool
		wantErr    bool
	}{
		{"production and enforced is fine", true, true, true, false},
		{"production and provably not enforced refuses", true, false, true, true},
		{"production and unknown refuses", true, false, false, true},
		{"dev and not enforced is allowed", false, false, true, false},
		{"dev and unknown is allowed", false, false, false, false},
		{"enforced wins over unknown", true, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := StorageCapGuardError(tc.production, tc.enforced, tc.known, "detail")
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

func TestCheckStorageCapUnsupportedRuntimeIsUnknown(t *testing.T) {
	t.Parallel()
	enforced, known, _ := CheckStorageCap(t.Context(), struct{}{})
	if enforced || known {
		t.Fatalf("enforced=%v known=%v; a runtime with no verifier must be unknown and not enforcing", enforced, known)
	}
}
