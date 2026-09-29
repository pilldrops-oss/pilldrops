package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ControlPlane is a StorageProvider backed by the control plane's own store,
// reached with the node key a node already has. It lets a multi-node fleet
// share templates without object storage.
type ControlPlane struct {
	base   string
	key    string
	client *http.Client
}

var _ StorageProvider = (*ControlPlane)(nil)

// NewControlPlane targets baseURL using key (a node's NODE_API_KEY) as the bearer credential.
func NewControlPlane(baseURL, key string, client *http.Client) (*ControlPlane, error) {
	if baseURL == "" {
		return nil, errors.New("artifact: control-plane store needs a base URL")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("artifact: invalid control-plane URL %q", baseURL)
	}
	if key == "" {
		return nil, errors.New("artifact: control-plane store needs a credential")
	}
	if client == nil {
		// No overall timeout: artifacts are hundreds of megabytes. Header and
		// idle timeouts still make a dead control plane fail fast.
		client = &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   4,
			},
		}
	}
	// A copy, so the caller's client keeps its own redirect policy. The control
	// plane may redirect a read to a presigned bucket URL: the node key must
	// never follow it. Go keeps Authorization on a redirect to the same
	// hostname on another port, which is exactly a bucket beside the control
	// plane, and a bucket refuses a request carrying two credentials anyway.
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("artifact: stopped after 10 redirects")
		}
		req.Header.Del("Authorization")
		return nil
	}
	return &ControlPlane{base: strings.TrimRight(baseURL, "/"), key: key, client: &c}, nil
}

func (c *ControlPlane) Name() string { return "controlplane" }

func (c *ControlPlane) req(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.key)
	return r, nil
}

func (c *ControlPlane) keyPath(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	// Each segment is escaped separately so the slashes stay structural.
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return ArtifactPathPrefix + strings.Join(parts, "/"), nil
}

func (c *ControlPlane) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	p, err := c.keyPath(key)
	if err != nil {
		return err
	}
	req, err := c.req(ctx, http.MethodPut, p, r)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("artifact: put %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return statusErr(key, resp)
	}
	return nil
}

func (c *ControlPlane) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return c.fetch(ctx, key, "")
}

func (c *ControlPlane) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, fmt.Errorf("artifact: negative range for %q (off=%d length=%d)", key, off, length)
	}
	return c.fetch(ctx, key, rangeHeader(off, length))
}

func (c *ControlPlane) fetch(ctx context.Context, key, rng string) (io.ReadCloser, error) {
	p, err := c.keyPath(key)
	if err != nil {
		return nil, err
	}
	req, err := c.req(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifact: get %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		return nil, statusErr(key, resp)
	}
	return resp.Body, nil
}

func (c *ControlPlane) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	p, err := c.keyPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	req, err := c.req(ctx, http.MethodHead, p, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("artifact: stat %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ObjectInfo{}, statusErr(key, resp)
	}
	info := ObjectInfo{Key: key, Size: resp.ContentLength}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			info.Modified = t
		}
	}
	return info, nil
}

func (c *ControlPlane) Delete(ctx context.Context, key string) error {
	p, err := c.keyPath(key)
	if err != nil {
		return err
	}
	req, err := c.req(ctx, http.MethodDelete, p, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("artifact: delete %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return statusErr(key, resp)
	}
	return nil
}

func (c *ControlPlane) List(ctx context.Context, prefix string) ([]string, error) {
	req, err := c.req(ctx, http.MethodGet, ListPath+"?prefix="+url.QueryEscape(prefix), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifact: list %q: %w", prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(prefix, resp)
	}
	var out struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("artifact: decode list: %w", err)
	}
	return out.Keys, nil
}

// statusErr maps status codes back to the sentinel errors so callers can
// branch on ErrNotFound regardless of backend.
func statusErr(key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		msg = payload.Error
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s: %s", ErrInvalidKey, key, msg)
	default:
		return fmt.Errorf("artifact: %q: control plane returned %d: %s", key, resp.StatusCode, msg)
	}
}
