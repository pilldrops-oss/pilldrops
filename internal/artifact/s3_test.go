package artifact_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// TestS3SignatureKnownAnswer is the non-circular check on SigV4: AWS publishes
// this request and its signature in the "Authenticating Requests: Using the
// Authorization Header" example. A fake server that re-derived the signature
// with this same code would agree with any bug; this vector does not.
func TestS3SignatureKnownAnswer(t *testing.T) {
	t.Parallel()

	const (
		wantSig     = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
		emptyBodySH = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	)
	var seen string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Get("Authorization")
		return nil, fmt.Errorf("stop here, the header is what is under test")
	})}

	s, err := artifact.NewS3(artifact.S3Options{
		Endpoint: "https://s3.amazonaws.com", Bucket: "examplebucket", Region: "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		VirtualHost: true, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact.S3SignAtForTest(s, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC), emptyBodySH)
	_, _ = s.GetRange(t.Context(), "test.txt", 0, 10)

	if !strings.Contains(seen, "Signature="+wantSig) {
		t.Fatalf("signature does not match AWS's published example.\n got: %s\nwant Signature=%s", seen, wantSig)
	}
	for _, want := range []string{
		"Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request",
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date",
	} {
		if !strings.Contains(seen, want) {
			t.Errorf("Authorization is missing %q:\n%s", want, seen)
		}
	}
}

// A changed request must change the signature, or the vector above proves
// nothing about the canonical request actually covering the request.
func TestS3SignatureCoversTheRequest(t *testing.T) {
	t.Parallel()

	sig := func(key string, off int64) string {
		var seen string
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = r.Header.Get("Authorization")
			return nil, fmt.Errorf("stop")
		})}
		s, err := artifact.NewS3(artifact.S3Options{
			Endpoint: "https://s3.amazonaws.com", Bucket: "b", Region: "us-east-1",
			AccessKey: "AK", SecretKey: "SK", VirtualHost: true, HTTPClient: client,
		})
		if err != nil {
			t.Fatal(err)
		}
		artifact.S3SignAtForTest(s, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC), "")
		_, _ = s.GetRange(t.Context(), key, off, 10)
		return seen
	}
	base := sig("a/b", 0)
	if base == sig("a/c", 0) {
		t.Error("the path is not covered by the signature")
	}
	if base == sig("a/b", 5) {
		t.Error("the Range header is not covered by the signature")
	}
}

func TestS3Conformance(t *testing.T) {
	t.Parallel()
	runConformance(t, func(t *testing.T) artifact.StorageProvider {
		t.Helper()
		return newFakeS3Store(t)
	})
}

// TestS3ConformanceLive runs the same suite against a real S3-protocol server
// (MinIO, R2). Gated on credentials; skipped in CI, run on the node.
func TestS3ConformanceLive(t *testing.T) {
	ep := os.Getenv("PD_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("set PD_TEST_S3_ENDPOINT/_BUCKET/_ACCESS_KEY/_SECRET_KEY to run against a real store")
	}
	runConformance(t, func(t *testing.T) artifact.StorageProvider {
		t.Helper()
		s, err := artifact.NewS3(artifact.S3Options{
			Endpoint:  ep,
			Bucket:    os.Getenv("PD_TEST_S3_BUCKET"),
			Region:    os.Getenv("PD_TEST_S3_REGION"),
			AccessKey: os.Getenv("PD_TEST_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("PD_TEST_S3_SECRET_KEY"),
		})
		if err != nil {
			t.Fatal(err)
		}
		// Each subtest wants an empty store, and a real bucket is shared.
		keys, err := s.List(t.Context(), "")
		if err != nil {
			t.Fatalf("list the bucket (credentials or endpoint wrong?): %v", err)
		}
		for _, k := range keys {
			_ = s.Delete(t.Context(), k)
		}
		return s
	})
}

func TestNewS3RefusesIncompleteConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		o    artifact.S3Options
		want string
	}{
		{"no endpoint", artifact.S3Options{Bucket: "b", AccessKey: "a", SecretKey: "s"}, "endpoint"},
		{"no bucket", artifact.S3Options{Endpoint: "https://h", AccessKey: "a", SecretKey: "s"}, "bucket"},
		{"no keys", artifact.S3Options{Endpoint: "https://h", Bucket: "b"}, "access key"},
		{"bad scheme", artifact.S3Options{Endpoint: "s3://h", Bucket: "b", AccessKey: "a", SecretKey: "s"}, "endpoint"},
	} {
		_, err := artifact.NewS3(tc.o)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
}

// Open must build the real backend now, not refuse it.
func TestOpenS3(t *testing.T) {
	t.Parallel()
	s, err := artifact.Open(artifact.KindS3, artifact.Options{S3: artifact.S3Options{
		Endpoint: "https://s3.example", Bucket: "b", AccessKey: "a", SecretKey: "s",
	}})
	if err != nil || s.Name() != "s3" {
		t.Fatalf("Open(s3) = %v, %v; want an s3 store", s, err)
	}
	if !artifact.Shared(artifact.KindS3) {
		t.Error("s3 must count as shared, or a clustered node refuses to boot with it")
	}
	ks := strings.Join(artifact.Kinds(), ",")
	if !strings.Contains(ks, "s3") {
		t.Errorf("Kinds() = %s, want s3 listed", ks)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeS3 is an in-process S3-protocol server: enough of PUT/GET/HEAD/DELETE
// and ListObjectsV2 to run the conformance suite without Docker. It checks
// that a request is signed but not that the signature is right — that is what
// the known-answer test and the live MinIO run are for.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	bucket  string
}

func newFakeS3Store(t *testing.T) artifact.StorageProvider {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, bucket: "pdtest"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := artifact.NewS3(artifact.S3Options{
		Endpoint: srv.URL, Bucket: f.bucket, Region: "us-east-1",
		AccessKey: "AK", SecretKey: "SK", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
		writeS3Error(w, http.StatusForbidden, "SignatureDoesNotMatch", "unsigned request")
		return
	}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/"+f.bucket)
	key, err := decodeSegments(strings.TrimPrefix(path, "/"))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidURI", err.Error())
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if key == "" && r.Method == http.MethodGet {
		f.list(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		// A real PUT publishes only on a complete body: the transport aborts
		// the request when the reader fails, and nothing lands.
		body, rerr := io.ReadAll(r.Body)
		if rerr != nil || (r.ContentLength >= 0 && int64(len(body)) != r.ContentLength) {
			writeS3Error(w, http.StatusBadRequest, "IncompleteBody", "short body")
			return
		}
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		body, ok := f.objects[key]
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey", key)
			return
		}
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		if rng := r.Header.Get("Range"); rng != "" && r.Method == http.MethodGet {
			off, last, ok := parseTestRange(rng)
			if !ok || off > int64(len(body)) {
				writeS3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", rng)
				return
			}
			end := min(last+1, int64(len(body)))
			seg := body[off:end]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, end-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(seg)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(seg)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

// list answers ListObjectsV2, paginating at two keys so the continuation-token
// loop is exercised by the ordinary conformance run.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if tok := r.URL.Query().Get("continuation-token"); tok != "" {
		for len(keys) > 0 && keys[0] <= tok {
			keys = keys[1:]
		}
	}
	type content struct {
		Key string `xml:"Key"`
	}
	out := struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
	}{}
	if len(keys) > 2 {
		out.IsTruncated = true
		out.NextContinuationToken = keys[1]
		keys = keys[:2]
	}
	for _, k := range keys {
		out.Contents = append(out.Contents, content{Key: k})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}

func writeS3Error(w http.ResponseWriter, code int, s3code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", s3code, msg)
}

// decodeSegments undoes the per-segment percent-encoding the signer applies.
func decodeSegments(p string) (string, error) {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		var b bytes.Buffer
		for i := 0; i < len(seg); i++ {
			if seg[i] != '%' {
				b.WriteByte(seg[i])
				continue
			}
			if i+2 >= len(seg) {
				return "", fmt.Errorf("truncated escape in %q", p)
			}
			v, err := strconv.ParseUint(seg[i+1:i+3], 16, 8)
			if err != nil {
				return "", err
			}
			b.WriteByte(byte(v))
			i += 2
		}
		out = append(out, b.String())
	}
	return strings.Join(out, "/"), nil
}

func parseTestRange(h string) (off, last int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	off, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	return off, last, err1 == nil && err2 == nil && last >= off
}

// errAfter yields n bytes of data and then fails, like a node dying mid-upload.
type errAfter struct{ n int }

func (e *errAfter) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, errors.New("source died")
	}
	if len(p) > e.n {
		p = p[:e.n]
	}
	for i := range p {
		p[i] = byte(i)
	}
	e.n -= len(p)
	return len(p), nil
}

// A body of unknown size larger than one part streams as a multipart upload,
// a failed one leaves no object behind, and a presigned URL serves the object
// (and a range of it) to a client holding no credential.
func TestS3MultipartAndPresignLive(t *testing.T) {
	ep := os.Getenv("PD_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("set PD_TEST_S3_ENDPOINT/_BUCKET/_ACCESS_KEY/_SECRET_KEY to run against a real store")
	}
	s, err := artifact.NewS3(artifact.S3Options{
		Endpoint: ep, Bucket: os.Getenv("PD_TEST_S3_BUCKET"), Region: os.Getenv("PD_TEST_S3_REGION"),
		AccessKey: os.Getenv("PD_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("PD_TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	want := make([]byte, 40<<20+123) // three parts, the last one short
	for i := range want {
		want[i] = byte(i * 7)
	}
	if err := s.Put(ctx, "mp/big", bytes.NewReader(want), -1); err != nil {
		t.Fatalf("multipart put: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(context.Background(), "mp/big") })
	rc, err := s.Get(ctx, "mp/big")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("read back %d bytes, want %d identical", len(got), len(want))
	}

	if err := s.Put(ctx, "mp/broken", &errAfter{n: 20 << 20}, -1); err == nil {
		t.Fatal("a put whose source died reported success")
	}
	if _, err := s.Stat(ctx, "mp/broken"); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("a failed multipart put left an object behind: %v", err)
	}

	u, err := s.Presign(http.MethodGet, "mp/big", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, want) {
		t.Fatalf("presigned GET = %d with %d bytes", resp.StatusCode, len(got))
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Range", "bytes=100-199")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, want[100:200]) {
		t.Fatalf("presigned ranged GET = %d with %d bytes", resp.StatusCode, len(got))
	}
}

// A missing bucket reads as a configuration error, never as a missing object.
func TestS3MissingBucketIsNotAMissingObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist.</Message></Error>`))
	}))
	defer srv.Close()
	s, err := artifact.NewS3(artifact.S3Options{Endpoint: srv.URL, Bucket: "typo", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.List(t.Context(), "")
	if err == nil || errors.Is(err, artifact.ErrNotFound) || !strings.Contains(err.Error(), "bucket does not exist") {
		t.Fatalf("List on a missing bucket = %v, want a bucket error that is not ErrNotFound", err)
	}
	// Get carries the error body; Stat is a HEAD, which has none, so a 404
	// there cannot tell a missing bucket from a missing key. The boot probe
	// lists, which is why a typo'd bucket now fails loudly at start.
	if _, err := s.Get(t.Context(), "k"); err == nil || errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Get on a missing bucket = %v, read as a missing object", err)
	}
}

// Large uploads queue for a slot: never more than s3UploadsInFlight hold
// their buffers at once, however many callers put concurrently.
func TestS3BoundsConcurrentUploads(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>u</UploadId></InitiateMultipartUploadResult>`))
		case r.Method == http.MethodPut:
			w.Header().Set("ETag", `"e"`)
		case r.Method == http.MethodPost:
			mu.Lock()
			inFlight--
			mu.Unlock()
			_, _ = w.Write([]byte(`<CompleteMultipartUploadResult/>`))
		}
	}))
	defer srv.Close()
	s, err := artifact.NewS3(artifact.S3Options{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 17<<20) // just over one part: multipart
	body[0] = 1
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Put(t.Context(), fmt.Sprintf("k%d", i), bytes.NewReader(body), -1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak > 3 {
		t.Fatalf("%d multipart uploads ran at once, want at most 3", peak)
	}
}
