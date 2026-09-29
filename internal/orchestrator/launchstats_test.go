package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func statsServer(t *testing.T) (*Server, string) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "acme", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("fleet-key", "node-token")
	s.SetStore(st)
	return s, key
}

func statsReq(t *testing.T, s *Server, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

func statsFor(t *testing.T, s *Server, key, templates string) map[string]launchStat {
	t.Helper()
	w := statsReq(t, s, http.MethodGet, "/v1/launch-stats?templates="+templates, key, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET launch-stats = %d (body %q)", w.Code, w.Body.String())
	}
	var out struct {
		Stats map[string]launchStat `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Stats
}

func TestLaunchStatsWithholdTheMedianUntilThereIsEvidence(t *testing.T) {
	s, key := statsServer(t)

	for i := 0; i < minLaunchSamples-1; i++ {
		if w := statsReq(t, s, http.MethodPost, "/v1/launch-stats", key,
			fmt.Sprintf(`{"template":"node","ms":%d}`, 2000+i*100)); w.Code != http.StatusNoContent {
			t.Fatalf("record %d = %d", i, w.Code)
		}
	}
	got := statsFor(t, s, key, "node")["node"]
	if got.N != minLaunchSamples-1 {
		t.Errorf("N = %d, want %d", got.N, minLaunchSamples-1)
	}
	if got.P50Ms != 0 {
		t.Errorf("P50Ms = %d with only %d samples — a confident median off too little "+
			"data is worse than no estimate", got.P50Ms, got.N)
	}

	if w := statsReq(t, s, http.MethodPost, "/v1/launch-stats", key, `{"template":"node","ms":2500}`); w.Code != http.StatusNoContent {
		t.Fatalf("final record = %d", w.Code)
	}
	got = statsFor(t, s, key, "node")["node"]
	if got.N != minLaunchSamples {
		t.Errorf("N = %d, want %d", got.N, minLaunchSamples)
	}
	if got.P50Ms == 0 {
		t.Error("P50Ms still withheld at the threshold — the ETA never appears")
	}
}

// Omitting them would make "never seen" look identical to a typo.
func TestUnknownTemplatesStillAppearInTheAnswer(t *testing.T) {
	s, key := statsServer(t)

	stats := statsFor(t, s, key, "never-launched")
	got, ok := stats["never-launched"]
	if !ok {
		t.Fatal("an unseen template was omitted from the response")
	}
	if got.N != 0 || got.P50Ms != 0 {
		t.Errorf("unseen template = %+v, want zeroes", got)
	}
}

func TestLaunchStatsValidateTheirInput(t *testing.T) {
	s, key := statsServer(t)

	if w := statsReq(t, s, http.MethodGet, "/v1/launch-stats", key, ""); w.Code != http.StatusBadRequest {
		t.Errorf("GET with no templates = %d, want 400", w.Code)
	}
	if w := statsReq(t, s, http.MethodPost, "/v1/launch-stats", key, `{"ms":1000}`); w.Code != http.StatusBadRequest {
		t.Errorf("POST with no template = %d, want 400", w.Code)
	}
	many := make([]string, maxStatTemplates+10)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	stats := statsFor(t, s, key, strings.Join(many, ","))
	if len(stats) > maxStatTemplates {
		t.Errorf("asked for %d templates, got %d back — the cap is not applied", len(many), len(stats))
	}
}
