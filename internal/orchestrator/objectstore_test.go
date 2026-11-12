package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// bucketStub keeps whole objects by path; readOnly makes it refuse writes.
func bucketStub(t *testing.T, readOnly bool) string {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPut && readOnly:
			w.WriteHeader(http.StatusForbidden)
		case r.Method == http.MethodPut:
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && objects[r.URL.Path] != nil:
			_, _ = w.Write(objects[r.URL.Path])
		case r.Method == http.MethodDelete && objects[r.URL.Path] != nil:
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func operatorReq(s *Server, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer fleet-key")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func objectStoreBody(endpoint string) string {
	return `{"endpoint":"` + endpoint + `","bucket":"b","region":"us-east-1","access_key":"AKIAEXAMPLE1234","secret_key":"s3cr3t-value"}`
}

func TestObjectStoreSaveIsWriteOnly(t *testing.T) {
	s, _ := domainServer(t)
	if err := s.st.Load().SetSecretKey(strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	const path = "/v1/admin/object-store"
	if w := apiReq(t, s, "PUT", "u:one", path, objectStoreBody(bucketStub(t, false))); w.Code == http.StatusOK {
		t.Fatal("a customer saved the object store")
	}
	if w := operatorReq(s, "GET", path, ""); w.Code != http.StatusOK || decodeJSON(t, w)["configured"] != false {
		t.Fatalf("unset = %d %s", w.Code, w.Body)
	}
	if w := operatorReq(s, "PUT", path, objectStoreBody(bucketStub(t, true))); w.Code != http.StatusBadRequest || decodeJSON(t, w)["code"] != "object_store_unusable" {
		t.Fatalf("read-only bucket = %d %s, want it refused", w.Code, w.Body)
	}
	if p := s.artifacts.Load(); p != nil {
		t.Fatal("a refused bucket became the artifact store")
	}

	for _, w := range []*httptest.ResponseRecorder{
		operatorReq(s, "PUT", path, objectStoreBody(bucketStub(t, false))),
		operatorReq(s, "GET", path, ""),
	} {
		got := decodeJSON(t, w)
		if w.Code != http.StatusOK || got["source"] != "saved" || got["bucket"] != "b" || got["access_key_hint"] != "1234" {
			t.Fatalf("saved view = %d %s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "s3cr3t-value") || strings.Contains(w.Body.String(), "AKIAEXAMPLE1234") {
			t.Fatalf("the response carries a credential: %s", w.Body)
		}
	}
	if p := s.artifacts.Load(); p == nil || *p == nil {
		t.Fatal("the saved bucket is not the artifact store")
	}

	if w := operatorReq(s, "DELETE", path, ""); w.Code != http.StatusOK || decodeJSON(t, w)["configured"] != false {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if p := s.artifacts.Load(); p != nil {
		t.Fatal("the artifact store outlived its delete")
	}
}

func TestObjectStoreFromEnvRefusesASavedOne(t *testing.T) {
	s, _ := domainServer(t)
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.SetArtifactStore(blobs)
	const path = "/v1/admin/object-store"
	if w := operatorReq(s, "GET", path, ""); decodeJSON(t, w)["source"] != "env" {
		t.Fatalf("view = %s, want source env", w.Body)
	}
	if w := operatorReq(s, "PUT", path, objectStoreBody(bucketStub(t, false))); w.Code != http.StatusConflict {
		t.Fatalf("save over env = %d %s, want 409", w.Code, w.Body)
	}
}

func TestUseSavedObjectStoreAtBoot(t *testing.T) {
	s, _ := domainServer(t)
	st := s.st.Load()
	if err := st.SetSecretKey(strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if w := operatorReq(s, "PUT", "/v1/admin/object-store", objectStoreBody(bucketStub(t, false))); w.Code != http.StatusOK {
		t.Fatalf("save = %d %s", w.Code, w.Body)
	}
	restarted := NewServer("fleet-key", "nodetoken")
	restarted.UseSavedObjectStore(t.Context(), st)
	if p := restarted.artifacts.Load(); p == nil || *p == nil || !restarted.artifactsSaved.Load() {
		t.Fatal("a restarted control plane did not open the saved bucket")
	}
}
