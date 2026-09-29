package httpx

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Go client gets the exact bytes back whether or not the body was
// compressed, and a Range read is never compressed.
func TestCopyBodyRoundTrips(t *testing.T) {
	body := append(make([]byte, 1<<20), []byte("tail")...) // mostly zero, like a memfile
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = CopyBody(w, r, int64(len(body)), bytes.NewReader(body))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !resp.Uncompressed || !bytes.Equal(got, body) {
		t.Fatalf("compressed=%v, %d bytes back of %d, equal=%v", resp.Uncompressed, len(got), len(body), bytes.Equal(got, body))
	}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Range", "bytes=0-9")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" || resp.ContentLength != int64(len(body)) {
		t.Fatalf("a Range read was compressed (%q) or lost its length (%d)", resp.Header.Get("Content-Encoding"), resp.ContentLength)
	}
}
