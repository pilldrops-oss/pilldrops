package artifact_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// presigningFS is an fs store whose presigned URLs point at a separate "bucket".
type presigningFS struct {
	artifact.StorageProvider
	bucket string
}

func (p presigningFS) Presign(_, key string, _ time.Duration) (string, error) {
	return p.bucket + "/" + key + "?sig=ok", nil
}

// A node reading through the control plane is sent to the bucket for the
// bytes, keeps its Range, and never shows the bucket its node key.
func TestDirectReadsGoToTheBucketWithoutTheNodeKey(t *testing.T) {
	fs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(strings.Repeat("snapshot-bytes-", 1000))
	if err := fs.Put(context.Background(), "b1/memfile", bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatal(err)
	}
	var leaked, hits int
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") != "" {
			leaked++
		}
		artifact.NewReadOnlyHandler(fs).ServeHTTP(w, r.Clone(r.Context()))
	}))
	defer bucket.Close()

	h := artifact.NewReadOnlyHandler(presigningFS{fs, bucket.URL + "/v1/artifacts"})
	h.Direct = time.Minute
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer cp.Close()

	client, err := artifact.NewControlPlane(cp.URL, "node-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := client.Get(context.Background(), "b1/memfile")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d", len(got), len(want))
	}
	rc, err = client.GetRange(context.Background(), "b1/memfile", 15, 15)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != string(want[15:30]) {
		t.Fatalf("ranged read through the redirect = %q", got)
	}
	if hits != 2 {
		t.Errorf("the bucket served %d reads, want 2: the bytes did not go direct", hits)
	}
	if leaked != 0 {
		t.Errorf("the node key reached the bucket %d times", leaked)
	}
}
