package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Losers may get 409 (claim held) or a replayed 201; a second sandbox is the bug.
func TestConcurrentCreateWithSameIdemKeyMakesOneSandbox(t *testing.T) {
	srv, key := newTestServer(t)
	defer srv.Close()

	const racers = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created = map[string]bool{}
		codes   []int
	)
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			code, out := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key,
				`{"template":"python-3.11"}`, "same-key")
			mu.Lock()
			defer mu.Unlock()
			codes = append(codes, code)
			if id, ok := out["id"].(string); ok && id != "" {
				created[id] = true
			}
		}()
	}
	wg.Wait()

	if len(created) > 1 {
		t.Fatalf("one Idempotency-Key produced %d distinct sandboxes (%v) — every extra one runs, meters and bills untracked", len(created), created)
	}
	for _, c := range codes {
		if c != http.StatusCreated && c != http.StatusConflict {
			t.Errorf("status %d, want 201 (winner/replay) or 409 (claim held)", c)
		}
	}

	var list struct {
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &list)
	if len(list.Sandboxes) != 1 {
		t.Fatalf("node holds %d sandboxes, want 1", len(list.Sandboxes))
	}
}

func TestFailedCreateReleasesIdemKey(t *testing.T) {
	srv, key := newTestServer(t)
	defer srv.Close()

	// An unknown template is rejected after the claim is taken.
	code, _ := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key,
		`{"template":"no-such-template-xyz"}`, "retry-me")
	if code != http.StatusBadRequest {
		t.Fatalf("first attempt: status %d, want 400", code)
	}

	code, out := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key,
		`{"template":"python-3.11"}`, "retry-me")
	if code != http.StatusCreated {
		t.Fatalf("retry after a failed create: status %d, want 201 — the key was not released", code)
	}
	if id, _ := out["id"].(string); id == "" {
		t.Error("retry returned no sandbox id")
	}
}

func TestSequentialCreateRetryReplays(t *testing.T) {
	srv, key := newTestServer(t)
	defer srv.Close()

	code1, out1 := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key,
		`{"template":"python-3.11"}`, "replay-key")
	if code1 != http.StatusCreated {
		t.Fatalf("first create: status %d, want 201", code1)
	}
	code2, out2 := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key,
		`{"template":"python-3.11"}`, "replay-key")
	if code2 != http.StatusCreated {
		t.Fatalf("replay: status %d, want 201", code2)
	}
	if out1["id"] != out2["id"] {
		t.Errorf("replay returned a different sandbox: %v vs %v", out1["id"], out2["id"])
	}
}

// One key reused across a batch must not skip work on every sandbox after the first.
func TestScopedIdemKeySeparatesActionAndResource(t *testing.T) {
	if got := scopedIdemKey("pause", "sb-1", ""); got != "" {
		t.Errorf("no key supplied should stay empty, got %q", got)
	}
	pause1 := scopedIdemKey("pause", "sb-1", "k")
	pause2 := scopedIdemKey("pause", "sb-2", "k")
	destroy1 := scopedIdemKey("destroy", "sb-1", "k")
	if pause1 == pause2 {
		t.Error("same key on two sandboxes must not collide — the second pause would replay the first's response")
	}
	if pause1 == destroy1 {
		t.Error("same key for pause and destroy must not collide — destroy would replay pause's response")
	}
	if !strings.Contains(pause1, "\x00") {
		t.Error("want a NUL separator no caller-supplied id or key can contain")
	}
}
