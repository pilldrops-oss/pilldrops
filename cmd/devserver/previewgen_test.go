package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// The orchestrator binds public_preview into the token's generation, so the
// portal minting under visibility alone handed a public desktop sandbox a
// 6080 link the gate refused.
func TestRebrandMintsUnderThePublicGeneration(t *testing.T) {
	old := previewSecret
	previewSecret = []byte("pdpv:secret")
	t.Cleanup(func() { previewSecret = old })

	r := httptest.NewRequest("GET", "https://example.com/v1/sandboxes/sb1", nil)
	body := rebrandBody(r, []byte(`{"id":"sb1","template":"desktop","public_port":6080,"visibility":"org","public_preview":true}`))
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	tok, _ := m["cdp_token"].(string)
	if !previewtoken.BrowserValid(previewSecret, tok, "sb1", 6080, sandbox.PreviewGeneration("org", true, 0)) {
		t.Errorf("token %q does not validate under the public generation", tok)
	}
}

// After a flip the generation carries a count the portal has no store to read;
// minting from visibility and public_preview alone broke every 7681/6080 link
// (the ports a token is always needed on) on a sandbox that had ever flipped.
func TestRebrandMintsUnderTheRelayedGeneration(t *testing.T) {
	old := previewSecret
	previewSecret = []byte("pdpv:secret")
	t.Cleanup(func() { previewSecret = old })

	r := httptest.NewRequest("GET", "https://example.com/v1/sandboxes/sb1", nil)
	body := rebrandBody(r, []byte(`{"id":"sb1","template":"desktop","public_port":6080,"visibility":"org",`+
		`"public_preview":false,"preview_generation":"org#3"}`))
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	tok, _ := m["cdp_token"].(string)
	if !previewtoken.BrowserValid(previewSecret, tok, "sb1", 6080, "org#3") {
		t.Errorf("token %q does not validate under the relayed generation", tok)
	}
}
