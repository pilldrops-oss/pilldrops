package envdproto

import (
	"strconv"
	"testing"
)

func TestTagSpellsProtocol(t *testing.T) {
	if Tag != tagPrefix+strconv.Itoa(Protocol) {
		t.Fatalf("Tag = %q but Protocol = %d; bump both together", Tag, Protocol)
	}
	if Parse(Tag) != Protocol {
		t.Fatalf("Parse(Tag) = %d, want %d", Parse(Tag), Protocol)
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]int{
		"1.0.0":            1, // every envd before levels existed
		"":                 1,
		"envd-protocol=7":  7,
		"envd-protocol=0":  1,
		"envd-protocol=x":  1,
		"envd-protocol=12": 12,
	} {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestScan(t *testing.T) {
	for name, tc := range map[string]struct {
		bin  string
		want int
	}{
		"tagged":                        {"\x00junk envd-protocol=5\x00more", 5},
		"tag wins over legacy marker":   {"SETHASH ... envd-protocol=3", 3},
		"vsock credential, before tags": {"\x00SETHASH\x00", VsockCredential},
		"older than both":               {"\x00just an old envd\x00", 1},
	} {
		if got := Scan([]byte(tc.bin)); got != tc.want {
			t.Errorf("%s: Scan = %d, want %d", name, got, tc.want)
		}
	}
}
