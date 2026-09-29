package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func TestWaitlistCreateAndListFromStore(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := NewServer("k", "t")
	s.SetStore(st)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	get := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := post("/v1/waitlist", `{"email":"a@b.com","source":"pricing"}`); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("create bad: %d %s", code, body)
	}
	if code, body := get("/v1/admin/waitlist"); code != 200 || !strings.Contains(body, "a@b.com") || !strings.Contains(body, "pricing") {
		t.Fatalf("list bad: %d %s", code, body)
	}

	if code, _ := post("/v1/waitlist", `{"email":"nope"}`); code != 400 {
		t.Fatalf("invalid email = %d, want 400", code)
	}

	rows, err := st.ListWaitlist(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListWaitlist: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (the invalid email must not persist)", len(rows))
	}

	if code, _ := func() (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/waitlist", strings.NewReader(`{"email":"c@d.com"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}(); code != 401 {
		t.Fatalf("unauth create = %d, want 401", code)
	}
	if code, _ := func() (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/waitlist", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}(); code != 401 {
		t.Fatalf("unauth list = %d, want 401", code)
	}
}

func TestWaitlistNoStoreNoNode(t *testing.T) {
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/waitlist", strings.NewReader(`{"email":"a@b.com"}`))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("create with no store/node = %d, want 503", resp.StatusCode)
	}
}
