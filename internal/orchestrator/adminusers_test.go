package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func TestAdminUsersProxiedToNode(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/users" {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"users":[{"owner_id":"u:abc"}],"count":1}`)
	}))
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/users", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "u:abc") {
		t.Fatalf("proxy bad: %d %s", resp.StatusCode, string(b))
	}
}

func TestAdminUsersFromStore(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.UpsertLogin(t.Context(), time.Now().Unix(), "u:abc", "user_abc", "abc@x.com"); err != nil {
		t.Fatalf("seed login: %v", err)
	}

	s := NewServer("k", "t")
	s.SetStore(st) // no node registered, so this must serve from the store

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

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

	if code, body := get("/v1/admin/users"); code != 200 || !strings.Contains(body, "u:abc") || !strings.Contains(body, "abc@x.com") {
		t.Fatalf("list from store bad: %d %s", code, body)
	}
	if code, body := get("/v1/admin/users/u:abc"); code != 200 || !strings.Contains(body, "u:abc") {
		t.Fatalf("detail from store bad: %d %s", code, body)
	}
	if code, _ := get("/v1/admin/users/u:nope"); code != 404 {
		t.Fatalf("unknown owner = %d, want 404", code)
	}
}

func TestLoginEventFromStore(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := NewServer("k", "t")
	s.SetStore(st)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/login-event",
		strings.NewReader(`{"owner_id":"u:xyz","user_id":"user_xyz","email":"xyz@x.com"}`))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("login-event bad: %d %s", resp.StatusCode, string(b))
	}

	users, err := st.ListUsers(t.Context(), 10)
	if err != nil {
		t.Fatalf("list after login: %v", err)
	}
	if len(users) != 1 || users[0].OwnerID != "u:xyz" {
		t.Fatalf("store did not record login: %+v", users)
	}
}

func TestAdminUsersNoHealthyNode(t *testing.T) {
	s := NewServer("k", "t") // no nodes
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/users", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("want 503 with no node, got %d", resp.StatusCode)
	}
}

// A sandbox the route table does not know (destroyed) must still reach a healthy node.
func TestAdminSandboxProxyFallsBackToHealthyNode(t *testing.T) {
	var hitPath string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"state":"destroyed","record":{"id":"sb-gone"}}`)
	}))
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/sandboxes/sb-gone", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || hitPath != "/v1/admin/sandboxes/sb-gone" || !strings.Contains(string(b), "sb-gone") {
		t.Fatalf("status=%d path=%q body=%s", resp.StatusCode, hitPath, string(b))
	}

	req2, _ := http.NewRequest("GET", srv.URL+"/v1/admin/sandboxes/sb-gone", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("unauth = %d, want 401", resp2.StatusCode)
	}
}

func TestAdminSandboxProxyDegradesWhenOwningNodeDown(t *testing.T) {
	// Registered and within heartbeat TTL, but the port is closed.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"state":"running","record":{"id":"sb-owned"}}`)
	}))
	defer alive.Close()

	s := NewServer("k", "t")
	now := time.Now()
	upsert(s.reg, "node-dead", deadURL, "kk", "", nil, "", cluster.Capacity{}, now)
	upsert(s.reg, "node-alive", alive.URL, "kk", "", nil, "", cluster.Capacity{}, now)
	s.reg.Place("sb-owned", "node-dead") // route table says node-dead owns it

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/sandboxes/sb-owned", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "sb-owned") {
		t.Fatalf("degrade to healthy node failed: status=%d body=%s", resp.StatusCode, string(b))
	}
}
