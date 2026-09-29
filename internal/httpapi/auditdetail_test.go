package httpapi

import (
	"strings"
	"testing"
)

// Env values must never reach the audit trail.
func TestEnvKeysNeverLeakValues(t *testing.T) {
	got := envKeys([]string{"API_KEY=sekrit-value", "EMPTY=", "BARE", "=leaked-value", "="})
	want := []string{"API_KEY", "EMPTY", "BARE"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if strings.Contains(strings.Join(got, ","), "sekrit") {
		t.Fatalf("value leaked: %v", got)
	}
	if strings.Contains(strings.Join(got, ","), "leaked") {
		t.Fatalf("value leaked: %v", got)
	}

	m := sortedKeys(map[string]string{"B_KEY": "v2", "A_KEY": "v1"})
	if len(m) != 2 || m[0] != "A_KEY" || m[1] != "B_KEY" {
		t.Fatalf("map keys = %v (want sorted [A_KEY B_KEY])", m)
	}
}
