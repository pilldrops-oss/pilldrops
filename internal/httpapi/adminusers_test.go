package httpapi

import "testing"

func TestLoginEventEndpoint(t *testing.T) {
	srv, key := newAdminServer(t)

	if code, out := do(t, "POST", srv.URL+"/v1/admin/login-event", key,
		`{"owner_id":"u:abc","user_id":"abc","email":"a@b.com"}`); code != 200 || out["ok"] != true {
		t.Fatalf("login-event = %d %v", code, out)
	}
	if c, _ := do(t, "POST", srv.URL+"/v1/admin/login-event", key, `{"email":"x"}`); c != 400 {
		t.Fatalf("missing owner_id = %d, want 400", c)
	}
	if c, _ := do(t, "POST", srv.URL+"/v1/admin/login-event", "", `{"owner_id":"u:z"}`); c != 401 {
		t.Fatalf("unauth = %d, want 401", c)
	}
}

func TestAdminUsersListAndDetail(t *testing.T) {
	srv, key := newAdminServer(t)

	if code, _ := do(t, "POST", srv.URL+"/v1/admin/login-event", key,
		`{"owner_id":"u:abc","user_id":"abc","email":"a@b.com"}`); code != 200 {
		t.Fatalf("seed login = %d", code)
	}

	// Caller org is "default" and still sees u:abc.
	code, out := do(t, "GET", srv.URL+"/v1/admin/users", key, "")
	if code != 200 {
		t.Fatalf("list = %d %v", code, out)
	}
	users, _ := out["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("want 1 user, got %v", out["users"])
	}
	u0 := users[0].(map[string]any)
	if u0["owner_id"] != "u:abc" || u0["email"] != "a@b.com" {
		t.Fatalf("bad user row: %v", u0)
	}

	code, det := do(t, "GET", srv.URL+"/v1/admin/users/u:abc", key, "")
	if code != 200 {
		t.Fatalf("detail = %d %v", code, det)
	}
	if user, _ := det["user"].(map[string]any); user["owner_id"] != "u:abc" {
		t.Fatalf("bad detail user: %v", det["user"])
	}

	if c, _ := do(t, "GET", srv.URL+"/v1/admin/users/u:nope", key, ""); c != 404 {
		t.Fatalf("unknown owner = %d, want 404", c)
	}

	if c, _ := do(t, "GET", srv.URL+"/v1/admin/users", "", ""); c != 401 {
		t.Fatalf("unauth list = %d, want 401", c)
	}
}
