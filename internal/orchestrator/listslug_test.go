package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// Every verb that takes an id takes a slug instead, and the list is where a
// caller goes to find out which sandbox is which. It carried everything about a
// row but that one field, so the only way to learn a slug was to already know
// the id and ask for the sandbox by name — and `sandbox ls | grep myapp` found
// nothing on a sandbox that has one.
func TestTheListCarriesTheSlugItsRowsCanBeAddressedBy(t *testing.T) {
	orch, _, keyA, _, _ := slugFleet(t)

	req, _ := http.NewRequest(http.MethodGet, orch.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+keyA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d: %s", resp.StatusCode, body)
	}

	var page struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	slugs := map[string]string{}
	for _, row := range page.Sandboxes {
		id, _ := row["id"].(string)
		slug, _ := row["slug"].(string)
		slugs[id] = slug
		// The row's lifecycle field, which the CLI renders as its STATUS
		// column. An empty one prints an empty column for every sandbox.
		if s, _ := row["state"].(string); s == "" {
			t.Errorf("row %s carries no state", id)
		}
	}
	for id, want := range map[string]string{"sb1": "myapp", "sb2": "web"} {
		if slugs[id] != want {
			t.Errorf("list row %s has slug %q, want %q", id, slugs[id], want)
		}
	}
	// A sandbox with no slug must not grow an empty one.
	if _, ok := page.Sandboxes[0]["slug"]; ok && slugs["web"] != "" {
		t.Errorf("sandbox web has slug %q and was never given one", slugs["web"])
	}
}
