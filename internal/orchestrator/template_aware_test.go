package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The 400 must name what is bootable rather than forwarding to a node that 503s.
func TestCreateUnsupportedTemplateFailsFastWith400(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"sb-1","status":"running"}`))
	}))
	defer node.Close()

	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	hbWith(t, srv.URL, `{"id":"n1","url":"`+node.URL+`","runtime":"firecracker","isolation":true,`+
		`"templates":["code-interpreter","base"],"capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`)

	create := func(body string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer apikey")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		return resp, string(rb)
	}

	resp, body := create(`{"template":"playwright"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("create(playwright) = %d (%s), want 400", resp.StatusCode, body)
	}
	for _, want := range []string{"playwright", "code-interpreter"} {
		if !strings.Contains(body, want) {
			t.Fatalf("400 body must mention %q: %s", want, body)
		}
	}

	// Aliases resolve before matching: python-3.11 is code-interpreter.
	resp, body = create(`{"template":"python-3.11"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create(python-3.11 → code-interpreter) = %d (%s), want 201", resp.StatusCode, body)
	}

	// An image-backed template needs a docker node.
	resp, body = create(`{"template":"postgres","image":"postgres:16"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("create(image-backed, fc-only fleet) = %d (%s), want 400", resp.StatusCode, body)
	}
	if !strings.Contains(body, "docker") {
		t.Fatalf("image-backed 400 must explain the docker-runtime requirement: %s", body)
	}
}

// A node that advertises no template list accepts any template.
func TestCreateWildcardNodeAcceptsAnyTemplate(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"sb-2","status":"running"}`))
	}))
	defer node.Close()

	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	hbWith(t, srv.URL, `{"id":"n2","url":"`+node.URL+`","runtime":"docker","isolation":true,`+
		`"capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", strings.NewReader(`{"template":"playwright"}`))
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rb, &out)
	if resp.StatusCode != 201 || out.ID == "" {
		t.Fatalf("create on wildcard node = %d (%s), want 201", resp.StatusCode, string(rb))
	}
}

func hbWith(t *testing.T, srvURL, body string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srvURL+"/internal/heartbeat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearerFor(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("heartbeat failed: %v %v", err, resp)
	}
	resp.Body.Close()
}
