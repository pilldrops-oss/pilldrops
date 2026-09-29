package artifact

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// s3PartSize is one multipart part. S3 wants at least 5 MiB for every part
	// but the last and allows 10,000 parts, so objects up to ~160 GiB fit.
	s3PartSize = 16 << 20
	// s3PartsInFlight bounds the parts uploading at once, and so the memory a
	// put holds: (s3PartsInFlight+1) * s3PartSize.
	s3PartsInFlight = 4
	s3MaxParts      = 10_000
	// s3UploadsInFlight bounds the large uploads one store runs at once, and
	// so a node's memory: measured on the rig, ten sandboxes pausing together
	// took the node daemon from 36 MiB to 1.3 GiB, each upload holding its
	// part buffers, while the link was what limited the time. A queued upload
	// waits for a slot rather than allocating.
	// ponytail: a constant; make it a knob if a fat link wants more.
	s3UploadsInFlight = 3
)

// putMultipart streams body as a multipart upload whose first part is first.
// Any failure aborts the upload, so no half-object and no orphaned parts are
// left for the bucket to bill.
func (s *S3) putMultipart(ctx context.Context, key, u string, first []byte, body io.Reader) error {
	uploadID, err := s.createMultipart(ctx, key, u)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu     sync.Mutex
		etags  []string
		upErr  error
		wg     sync.WaitGroup
		slots  = make(chan struct{}, s3PartsInFlight)
		failed = func(err error) {
			mu.Lock()
			if upErr == nil {
				upErr = err
				cancel()
			}
			mu.Unlock()
		}
	)
	upload := func(n int, part []byte) {
		defer wg.Done()
		defer func() { <-slots }()
		q := canonicalQuery([2]string{"partNumber", strconv.Itoa(n)}, [2]string{"uploadId", uploadID})
		resp, err := s.send(ctx, http.MethodPut, u, q, bytes.NewReader(part), int64(len(part)), nil)
		if err != nil {
			failed(fmt.Errorf("artifact: put %q part %d: %w", key, n, err))
			return
		}
		defer drain(resp)
		if resp.StatusCode != http.StatusOK {
			failed(s3Err(key, resp))
			return
		}
		mu.Lock()
		etags[n-1] = resp.Header.Get("ETag")
		mu.Unlock()
	}

	part := first
	for n := 1; part != nil; n++ {
		mu.Lock()
		stop := upErr != nil
		mu.Unlock()
		if stop {
			break
		}
		if n > s3MaxParts {
			failed(fmt.Errorf("artifact: %q is larger than %d parts of %d MiB", key, s3MaxParts, s3PartSize>>20))
			break
		}
		mu.Lock()
		etags = append(etags, "")
		mu.Unlock()
		slots <- struct{}{}
		wg.Add(1)
		go upload(n, part)

		next := make([]byte, s3PartSize)
		m, rerr := io.ReadFull(body, next)
		switch {
		case rerr == nil:
			part = next
		case rerr == io.ErrUnexpectedEOF:
			part = next[:m] // the short last part; the read after it answers EOF
		case rerr == io.EOF:
			part = nil
		default:
			failed(fmt.Errorf("artifact: read %q: %w", key, rerr))
			part = nil
		}
	}
	wg.Wait()
	if upErr != nil {
		s.abortMultipart(context.WithoutCancel(ctx), u, uploadID)
		return upErr
	}
	if err := s.completeMultipart(ctx, key, u, uploadID, etags); err != nil {
		s.abortMultipart(context.WithoutCancel(ctx), u, uploadID)
		return err
	}
	return nil
}

func (s *S3) createMultipart(ctx context.Context, key, u string) (string, error) {
	resp, err := s.send(ctx, http.MethodPost, u, canonicalQuery([2]string{"uploads", ""}), nil, 0, nil)
	if err != nil {
		return "", fmt.Errorf("artifact: start upload of %q: %w", key, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", s3Err(key, resp)
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil || out.UploadID == "" {
		return "", fmt.Errorf("artifact: start upload of %q: no upload id in the answer (%v)", key, err)
	}
	return out.UploadID, nil
}

func (s *S3) completeMultipart(ctx context.Context, key, u, uploadID string, etags []string) error {
	type part struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	doc := struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{}
	for i, e := range etags {
		doc.Parts = append(doc.Parts, part{PartNumber: i + 1, ETag: e})
	}
	body, _ := xml.Marshal(doc) //nolint:errcheck // a fixed struct of strings and ints
	resp, err := s.send(ctx, http.MethodPost, u, canonicalQuery([2]string{"uploadId", uploadID}),
		bytes.NewReader(body), int64(len(body)), http.Header{"Content-Type": {"application/xml"}})
	if err != nil {
		return fmt.Errorf("artifact: finish upload of %q: %w", key, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return s3Err(key, resp)
	}
	// S3 can answer 200 and put the failure in the body, having kept the
	// connection alive while it assembled the parts.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if bytes.Contains(raw, []byte("<Error>")) {
		return fmt.Errorf("artifact: finish upload of %q: %s", key, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (s *S3) abortMultipart(ctx context.Context, u, uploadID string) {
	resp, err := s.send(ctx, http.MethodDelete, u, canonicalQuery([2]string{"uploadId", uploadID}), nil, -1, nil)
	if err == nil {
		drain(resp)
	}
}

// Presign returns a URL that performs method on key with no credential until
// expiry. The control plane hands these to nodes, so a node downloads straight
// from the bucket without holding the bucket's keys.
func (s *S3) Presign(method, key string, expiry time.Duration) (string, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if s.clock != nil {
		now = s.clock()
	}
	stamp := now.Format("20060102T150405Z")
	day := stamp[:8]
	scope := day + "/" + s.opts.Region + "/s3/aws4_request"
	params := [][2]string{
		{"X-Amz-Algorithm", "AWS4-HMAC-SHA256"},
		{"X-Amz-Credential", s.opts.AccessKey + "/" + scope},
		{"X-Amz-Date", stamp},
		{"X-Amz-Expires", strconv.Itoa(int(expiry.Seconds()))},
		{"X-Amz-SignedHeaders", "host"},
	}
	if s.opts.SessionToken != "" {
		params = append(params, [2]string{"X-Amz-Security-Token", s.opts.SessionToken})
	}
	q := canonicalQuery(params...)
	canonicalReq := strings.Join([]string{
		method, parsed.EscapedPath(), q, "host:" + s.host + "\n", "host", unsignedPayload,
	}, "\n")
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", stamp, scope, hashHex([]byte(canonicalReq))}, "\n")
	sig := hex.EncodeToString(hmacRaw(s.signingKey(day), stringToSign))
	return u + "?" + q + "&X-Amz-Signature=" + sig, nil
}

// Presigner is a store that can hand out credential-free URLs for its objects.
type Presigner interface {
	Presign(method, key string, expiry time.Duration) (string, error)
}

var _ Presigner = (*S3)(nil)

// S3OptionsFromEnv reads <prefix>ENDPOINT, BUCKET, REGION, ACCESS_KEY,
// SECRET_KEY, SESSION_TOKEN and VIRTUAL_HOST, so the node and the control
// plane configure a bucket the same way under their own prefixes.
//
// Google Cloud Storage is reached the same way through its S3-compatible XML
// API: ENDPOINT=https://storage.googleapis.com, REGION=auto, and an HMAC key
// pair for ACCESS_KEY/SECRET_KEY.
func S3OptionsFromEnv(prefix string, getenv func(string) string) S3Options {
	return S3Options{
		Endpoint:     getenv(prefix + "ENDPOINT"),
		Bucket:       getenv(prefix + "BUCKET"),
		Region:       getenv(prefix + "REGION"),
		AccessKey:    getenv(prefix + "ACCESS_KEY"),
		SecretKey:    getenv(prefix + "SECRET_KEY"),
		SessionToken: getenv(prefix + "SESSION_TOKEN"),
		VirtualHost:  getenv(prefix+"VIRTUAL_HOST") == "1",
	}
}
