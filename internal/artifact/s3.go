package artifact

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 is a StorageProvider speaking the S3 REST protocol over net/http: SigV4
// signing, PUT/GET/HEAD/DELETE and ListObjectsV2. No vendor SDK — the protocol
// is five verbs and one signature, and an SDK would pull a dependency tree
// larger than this file to reach the same five.
//
// It works against AWS S3, Cloudflare R2, MinIO and anything else speaking the
// protocol; the differences live in S3Options, not in the code.
type S3 struct {
	opts   S3Options
	client *http.Client
	// scheme and host are the endpoint's, after bucket placement.
	scheme, host string
	// prefix is "/bucket" in path style and "" in virtual-hosted style.
	prefix string
	// clock and payload are pinned only by the known-answer signing test,
	// which needs AWS's published timestamp and body hash to reproduce it.
	clock   func() time.Time
	payload string
	// uploads bounds the multipart uploads this store runs at once; see
	// s3UploadsInFlight.
	uploads chan struct{}
}

var _ StorageProvider = (*S3)(nil)

// S3Options configures the S3-protocol backend.
type S3Options struct {
	// Endpoint is the service URL, e.g. https://s3.us-east-1.amazonaws.com,
	// https://<account>.r2.cloudflarestorage.com or http://127.0.0.1:9000.
	Endpoint string
	Bucket   string
	// Region is the SigV4 region. R2 and MinIO accept "auto" / "us-east-1".
	Region    string
	AccessKey string
	SecretKey string
	// SessionToken is set for temporary credentials; usually empty.
	SessionToken string
	// VirtualHost addresses the bucket as a subdomain of the endpoint host
	// instead of the first path segment. Path style (the default) is what
	// MinIO and R2 want; AWS accepts both.
	VirtualHost bool
	// HTTPClient is optional.
	HTTPClient *http.Client
}

// unsignedPayload tells SigV4 not to hash the body. Streaming a multi-gigabyte
// disk twice — once to hash, once to send — is the alternative, and every
// S3-protocol server accepts this sentinel.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// NewS3 validates the configuration up front: a node that cannot reach its
// object store should fail at boot, not on the first backup.
func NewS3(o S3Options) (*S3, error) {
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"endpoint", o.Endpoint}, {"bucket", o.Bucket},
		{"access key", o.AccessKey}, {"secret key", o.SecretKey},
	} {
		if strings.TrimSpace(f.val) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("artifact: s3 store needs %s", strings.Join(missing, ", "))
	}
	u, err := url.Parse(strings.TrimRight(o.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("artifact: invalid s3 endpoint %q: want http(s)://host[:port]", o.Endpoint)
	}
	if strings.ContainsAny(o.Bucket, "/?#") {
		return nil, fmt.Errorf("artifact: invalid s3 bucket %q", o.Bucket)
	}
	if o.Region == "" {
		o.Region = "us-east-1"
	}
	s := &S3{opts: o, client: o.HTTPClient, scheme: u.Scheme, host: u.Host,
		uploads: make(chan struct{}, s3UploadsInFlight)}
	// The endpoint may carry a path (MinIO behind a reverse proxy); the bucket
	// sits after it in path style.
	s.prefix = strings.TrimRight(u.Path, "/")
	if o.VirtualHost {
		s.host = o.Bucket + "." + u.Host
	} else {
		s.prefix += "/" + o.Bucket
	}
	if s.client == nil {
		// No overall timeout: a disk image is gigabytes. Header and idle
		// timeouts still make a dead endpoint fail fast.
		s.client = &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
		}}
	}
	return s, nil
}

func (s *S3) Name() string { return "s3" }

// objectURL is the request URL for a key, with every path segment escaped the
// way SigV4 expects. ValidateKey runs first: a key is caller-influenced.
func (s *S3) objectURL(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = uriEncode(p)
	}
	return s.scheme + "://" + s.host + s.prefix + "/" + strings.Join(parts, "/"), nil
}

// send signs and performs one request. query is already canonical (encoded,
// sorted), because SigV4 signs exactly the string that goes on the wire.
func (s *S3) send(ctx context.Context, method, rawURL, query string, body io.Reader, length int64, hdr http.Header) (*http.Response, error) {
	full := rawURL
	if query != "" {
		full += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if length >= 0 {
		req.ContentLength = length
	}
	now := time.Now().UTC()
	if s.clock != nil {
		now = s.clock()
	}
	s.sign(req, query, now)
	return s.client.Do(req)
}

// sign adds the SigV4 Authorization header. Split out so a known-answer test
// can drive it against AWS's published example without a server.
func (s *S3) sign(req *http.Request, query string, now time.Time) {
	stamp := now.Format("20060102T150405Z")
	day := stamp[:8]

	req.Header.Set("X-Amz-Date", stamp)
	hash := unsignedPayload
	if s.payload != "" {
		hash = s.payload
	}
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if s.opts.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.opts.SessionToken)
	}
	req.Host = s.host

	// Canonical headers: lowercase names, sorted, values trimmed. Host plus
	// everything signing must cover; an unsigned header a proxy adds is fine.
	names := []string{"host"}
	vals := map[string]string{"host": req.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-amz-") && lk != "content-type" && lk != "range" {
			continue
		}
		names = append(names, lk)
		vals[lk] = strings.TrimSpace(strings.Join(v, ","))
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + vals[n] + "\n")
	}
	signed := strings.Join(names, ";")

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalReq := strings.Join([]string{
		req.Method, canonicalURI, query, ch.String(), signed, hash,
	}, "\n")

	scope := day + "/" + s.opts.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", stamp, scope, hashHex([]byte(canonicalReq)),
	}, "\n")

	sig := hex.EncodeToString(hmacRaw(s.signingKey(day), stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.opts.AccessKey, scope, signed, sig))
}

func (s *S3) signingKey(day string) []byte {
	return hmacRaw(hmacRaw(hmacRaw(hmacRaw([]byte("AWS4"+s.opts.SecretKey), day),
		s.opts.Region), "s3"), "aws4_request")
}

func hmacRaw(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// uriEncode is RFC 3986 unreserved-only escaping, which is what SigV4's
// canonical URI and query string need; net/url's escapers each leave a
// different set alone and none of them matches.
func uriEncode(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&0xf])
	}
	return b.String()
}

// canonicalQuery encodes and sorts parameters the SigV4 way. An empty value
// drops the parameter, except where the API requires it verbatim.
func canonicalQuery(kv ...[2]string) string {
	parts := make([]string, 0, len(kv))
	for _, p := range kv {
		if p[1] == "" && p[0] != "list-type" && p[0] != "uploads" {
			continue
		}
		parts = append(parts, uriEncode(p[0])+"="+uriEncode(p[1]))
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	u, err := s.objectURL(key)
	if err != nil {
		return err
	}
	body := newCtxReader(ctx, r)
	if size >= 0 && size <= s3PartSize {
		return s.putOnce(ctx, key, u, body, size)
	}
	// Unknown or large: a body that fits in one part is still one PUT, and
	// anything bigger streams as a multipart upload, since a single PUT stops
	// at 5 GiB and a memory image passes that. Each holds its part buffers,
	// so they queue for a slot before allocating any.
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	case <-ctx.Done():
		return ctx.Err()
	}
	first := make([]byte, s3PartSize)
	n, err := io.ReadFull(body, first)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return s.putOnce(ctx, key, u, bytes.NewReader(first[:n]), int64(n))
	}
	if err != nil {
		return fmt.Errorf("artifact: read %q: %w", key, err)
	}
	return s.putMultipart(ctx, key, u, first, body)
}

func (s *S3) putOnce(ctx context.Context, key, u string, body io.Reader, size int64) error {
	resp, err := s.send(ctx, http.MethodPut, u, "", body, size, nil)
	if err != nil {
		return fmt.Errorf("artifact: put %q: %w", key, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return s3Err(key, resp)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.fetch(ctx, key, "")
}

func (s *S3) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, fmt.Errorf("artifact: negative range for %q (off=%d length=%d)", key, off, length)
	}
	return s.fetch(ctx, key, rangeHeader(off, length))
}

func (s *S3) fetch(ctx context.Context, key, rng string) (io.ReadCloser, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if rng != "" {
		hdr.Set("Range", rng)
	}
	resp, err := s.send(ctx, http.MethodGet, u, "", nil, -1, hdr)
	if err != nil {
		return nil, fmt.Errorf("artifact: get %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer drain(resp)
		return nil, s3Err(key, resp)
	}
	return resp.Body, nil
}

func (s *S3) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	resp, err := s.send(ctx, http.MethodHead, u, "", nil, -1, nil)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("artifact: stat %q: %w", key, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return ObjectInfo{}, s3Err(key, resp)
	}
	info := ObjectInfo{Key: key, Size: resp.ContentLength}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, perr := http.ParseTime(lm); perr == nil {
			info.Modified = t
		}
	}
	return info, nil
}

// Delete stats first: S3 answers 204 for a key that was never there, and the
// contract says a missing key is ErrNotFound.
func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := s.Stat(ctx, key); err != nil {
		return err
	}
	u, err := s.objectURL(key)
	if err != nil {
		return err
	}
	resp, err := s.send(ctx, http.MethodDelete, u, "", nil, -1, nil)
	if err != nil {
		return fmt.Errorf("artifact: delete %q: %w", key, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return s3Err(key, resp)
	}
	return nil
}

// listResult is the subset of ListObjectsV2's XML this code reads.
type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	bucketURL := s.scheme + "://" + s.host + s.prefix + "/"
	var keys []string
	for token := ""; ; {
		q := canonicalQuery([2]string{"list-type", "2"}, [2]string{"prefix", prefix},
			[2]string{"continuation-token", token})
		resp, err := s.send(ctx, http.MethodGet, bucketURL, q, nil, -1, nil)
		if err != nil {
			return nil, fmt.Errorf("artifact: list %q: %w", prefix, err)
		}
		if resp.StatusCode != http.StatusOK {
			err := s3Err(prefix, resp)
			drain(resp)
			return nil, err
		}
		var lr listResult
		err = xml.NewDecoder(resp.Body).Decode(&lr)
		drain(resp)
		if err != nil {
			return nil, fmt.Errorf("artifact: decode list of %q: %w", prefix, err)
		}
		for _, c := range lr.Contents {
			keys = append(keys, c.Key)
		}
		if !lr.IsTruncated || lr.NextContinuationToken == "" {
			break
		}
		token = lr.NextContinuationToken
	}
	sort.Strings(keys)
	return keys, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// s3Err maps status codes onto the package's sentinels so callers branch the
// same way on every backend.
func s3Err(key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(body, &e)
	msg := strings.TrimSpace(e.Code + " " + e.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	switch {
	// A missing bucket is configuration, not a missing object: read as
	// ErrNotFound it said "artifact not found" at boot for a bucket name typo.
	case e.Code == "NoSuchBucket":
		return fmt.Errorf("artifact: the bucket does not exist (check the bucket name and the "+
			"credential's project): %s", msg)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("artifact: range not satisfiable for %q: %s", key, msg)
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("artifact: %q: s3 refused the credential (%d): %s", key, resp.StatusCode, msg)
	default:
		return fmt.Errorf("artifact: %q: s3 returned %d: %s", key, resp.StatusCode, msg)
	}
}
