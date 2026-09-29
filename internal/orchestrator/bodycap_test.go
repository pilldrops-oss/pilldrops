package orchestrator

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestBodyCap_CreateRejectsOversize(t *testing.T) {
	s := NewServer("apikey", "nodetoken")
	s.maxBodyBytes = 1 << 10
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	big := bytes.Repeat([]byte("x"), 4<<10)
	req, err := http.NewRequest("POST", srv.URL+"/v1/sandboxes", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("want 413, got %d", resp.StatusCode)
	}
}

// The request may still 503 with no node registered; only a 413 is ruled out.
func TestBodyCap_ImageBuildExemptFromGenericCap(t *testing.T) {
	s := NewServer("apikey", "nodetoken")
	s.maxBodyBytes = 1 << 10
	s.maxBuildContextBytes = 8 << 20
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := bytes.Repeat([]byte("x"), 2<<10)
	req, err := http.NewRequest("POST", srv.URL+"/v1/images/build", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 413 {
		t.Fatal("image build must be exempt from the generic body cap")
	}
}

// Without the exemption a body over the generic cap failed mid-stream as a 502.
// The fake node receiving the body byte-for-byte proves the path was exempted.
func TestBodyCap_SandboxProxyExemptFromGenericCap(t *testing.T) {
	var gotLen int
	var gotBody []byte
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		gotLen = len(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer node.Close()

	s := NewServer("apikey", "nodetoken")
	s.maxBodyBytes = 1 << 10
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	big := bytes.Repeat([]byte("x"), 8<<10)
	req, err := http.NewRequest("POST", srv.URL+"/v1/sandboxes/sb1/files?path=x", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 413 {
		t.Fatal("sandbox sub-resource proxy must be exempt from the generic body cap")
	}
	if resp.StatusCode == 502 {
		t.Fatal("sandbox sub-resource proxy must not fail mid-stream (502) due to the generic body cap")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy relay status = %d, want 200", resp.StatusCode)
	}
	if gotLen != len(big) || !bytes.Equal(gotBody, big) {
		t.Fatalf("node received %d bytes, want the full %d-byte body untruncated", gotLen, len(big))
	}
}

// The unknown sandbox 404s; only a 413 from capBody is ruled out.
func TestBodyCap_PreviewPathExemptFromGenericCap(t *testing.T) {
	s := NewServer("apikey", "nodetoken")
	s.maxBodyBytes = 1 << 10
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	big := bytes.Repeat([]byte("x"), 8<<10)
	resp, err := http.Post(srv.URL+"/preview/unknown-sandbox/8000/", "application/octet-stream", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 413 {
		t.Fatal("preview proxy path must be exempt from the generic body cap")
	}
}

func TestBodyCap_ImageBuildOwnCapRejectsOversize(t *testing.T) {
	s := NewServer("apikey", "nodetoken")
	s.maxBodyBytes = 10 << 20
	s.maxBuildContextBytes = 1 << 10
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := bytes.Repeat([]byte("x"), 4<<10)
	req, err := http.NewRequest("POST", srv.URL+"/v1/images/build", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("want 413 from handleImageBuild's own cap, got %d", resp.StatusCode)
	}
}

// A valid JSON body padded past handleJoin's own 4 KiB cap: the nested cap
// truncates the decode (400); if the outer cap governed, the bogus token would 401.
func TestBodyCap_JoinCapStillGoverns(t *testing.T) {
	s := NewServer("apikey", "nodetoken")
	s.maxBodyBytes = 10 << 20
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	padding := strings.Repeat("a", 8<<10)
	body := `{"token":"` + padding + `"}`
	req, err := http.NewRequest("POST", srv.URL+"/v1/join", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Proto", "https") // handleJoin requires HTTPS
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400 (truncated by handleJoin's 4 KiB cap), got %d — the tighter nested cap may not be governing", resp.StatusCode)
	}
}
