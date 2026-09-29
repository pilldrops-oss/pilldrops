package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/observability"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
)

// orgOverride carries the customer org a request acts for. The portal sends it
// bare; this server signs it before relaying so the node can verify it.
const orgOverride = "X-Org-Override"

// signedOrgOverride turns a bare org into a short-lived signed assertion bound
// to nodeID, method and path so a captured header cannot be replayed elsewhere.
// Prefer Ed25519 (ORG_ASSERT_KEY); the HMAC fallback hands every node the
// minting key. ok=false means refuse to relay, never send the bare string.
// principal is the calling key's id and role, bound into the same signature so
// the node can attribute what it creates; empty when no key was named.
func (s *Server) signedOrgOverride(org, nodeID, method, path string, principal ...string) (header string, ok bool) {
	if org == "" {
		return "", true
	}
	if s.orgAssertKey != nil {
		if tok := orgassert.SignAsym(s.orgAssertKey, org, nodeID, method, path, s.now(), principal...); tok != "" {
			return tok, true
		}
	}
	if tok := orgassert.Sign([]byte(s.nodeToken), org, nodeID, method, path, s.now(), principal...); tok != "" {
		return tok, true
	}
	if devInsecure() {
		return org, true // dev-only: legacy bare relay, never in production
	}
	return "", false
}

// setOrgAndKey signs the org assertion for one outbound request and, when the
// caller was named, sets the key headers the assertion covers. Every node-bound
// request goes through here, so no forwarding path can forget the creator and
// silently attribute a sandbox to nobody.
func (s *Server) setOrgAndKey(ctx context.Context, req *http.Request, node *cluster.Node, method, path, org string) error {
	if org == "" {
		return nil
	}
	keyID, keyRole := callerKey(ctx)
	var principal []string
	if keyID != "" {
		principal = []string{keyID, keyRole}
	}
	hdr, ok := s.signedOrgOverride(org, node.ID, method, path, principal...)
	if !ok {
		return errNoOrgOverrideSecret
	}
	if hdr == "" {
		return nil
	}
	req.Header.Set(orgOverride, hdr)
	if keyID != "" {
		req.Header.Set(keyIDHeader, keyID)
		req.Header.Set(keyRoleHeader, keyRole)
	}
	return nil
}

// relayClientHeaders is the allowlist of inbound headers the sandbox proxy
// carries to the node. Authorization and Cookie hold the orchestrator's own
// credentials and must never reach a node running customer code.
var relayClientHeaders = []string{
	"Last-Event-ID",
	"Idempotency-Key",
	"Accept",
	"Range",
	"X-Agent-Token",
	observability.HeaderRequestID,
}

// newStreamClient builds the client for proxied sandbox sub-paths. No overall
// Timeout, since that would kill long-lived streams mid-frame; redirects are
// relayed rather than followed so their Set-Cookie survives.
func newStreamClient() *http.Client {
	return &http.Client{
		// A literal &http.Transport{} has no dial timeout.
		Transport: httpx.NewTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// forwardStream issues an authenticated request to a node with the body
// streamed, not buffered. client is the inbound header set (nil relays none);
// an optional org is relayed as a signed X-Org-Override.
func (s *Server) forwardStream(ctx context.Context, node *cluster.Node, method, path string, body io.Reader, ctype string, client http.Header, org ...string) (*http.Response, error) {
	return s.forwardStreamRelaying(ctx, node, method, path, body, ctype, client, relayClientHeaders, org...)
}

// forwardStreamRelaying is forwardStream with an explicit header allowlist.
// The list is a security boundary: the agent origin relays X-Forwarded-Host and
// X-Agent-Token, which the shared API host must never let a caller assert.
func (s *Server) forwardStreamRelaying(ctx context.Context, node *cluster.Node, method, path string, body io.Reader, ctype string, client http.Header, relay []string, org ...string) (*http.Response, error) {
	base, hop, err := s.nodeHop(node, hopStream)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	if node.Key != "" {
		req.Header.Set("Authorization", "Bearer "+node.Key)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for _, h := range relay {
		if v := client.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	observability.InjectTrace(ctx, req.Header)
	if len(org) > 0 {
		if err := s.setOrgAndKey(ctx, req, node, method, path, org[0]); err != nil {
			return nil, err
		}
	}
	return hop.Do(req)
}

// errNoOrgOverrideSecret is returned when signedOrgOverride refuses to relay.
// errNodeUnreachable is a forward refused because the node has gone quiet.
var errNodeUnreachable = errors.New("node unreachable")

var errNoOrgOverrideSecret = errors.New("cannot relay X-Org-Override: orchestrator has no node token configured to sign it (set ORCHESTRATOR_TOKEN, or DEV_INSECURE=1 for local dev)")

// forwardRetries is how many extra attempts forward makes on a transport error.
const forwardRetries = 2

// doWithRetryIf calls do up to attempts+1 times, retrying only on a transport
// error the predicate allows to be replayed. A received response, even a 5xx,
// is never retried: the node may have executed it. backoff(i) is the delay
// before attempt i (i>=1), and the wait is interruptible so a gone caller does
// not pin a buffered body for the rest of the schedule.
func doWithRetryIf(ctx context.Context, do func() (*http.Response, error), attempts int, backoff func(int) time.Duration, retryable func(error) bool) (*http.Response, error) {
	var lastErr error
	for i := 0; i <= attempts; i++ {
		if i > 0 && backoff != nil {
			if !sleepCtx(ctx, backoff(i)) {
				// Caller is gone; report the transport error, not the cancellation.
				return nil, lastErr
			}
		}
		resp, err := do()
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(err) {
			break
		}
	}
	return nil, lastErr
}

// sleepCtx waits for d, reporting false if ctx finished first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// retryBackoff is the delay before retry attempt i (1-based). Equal jitter, so
// a node restart that fails every in-flight request at once is not met with a
// synchronized dial storm, and no draw lands near zero.
func retryBackoff(i int) time.Duration {
	base := time.Duration(i) * 100 * time.Millisecond
	return base/2 + rand.N(base/2)
}

// notDelivered reports whether err proves the request never reached the node.
// Only a dial failure qualifies; a timeout or reset may mean the node executed
// the request and a replay would duplicate a sandbox and its charge.
func notDelivered(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// forward issues an authenticated request to a node, retrying transport
// failures only for replay-safe methods. A timed-out POST create may have
// executed on the node, so a blind retry would produce untracked sandboxes.
func (s *Server) forward(ctx context.Context, node *cluster.Node, method, path string, body []byte, ctype string, org ...string) (*http.Response, error) {
	// A node that stopped heartbeating is refused at once. Dialled instead,
	// a partitioned node held each request for the client's whole timeout,
	// times every retry: minutes per request, measured, while the fleet
	// still showed it as the sandbox's home.
	if seen, quiet := s.nodeWentQuiet(node); quiet {
		return nil, fmt.Errorf("%w: %s last heartbeat %s ago", errNodeUnreachable, node.ID,
			s.now().Sub(seen).Round(time.Second))
	}
	once := func() (*http.Response, error) {
		return s.forwardOnce(ctx, node, method, path, body, ctype, org...)
	}
	if replaySafe(method) {
		return doWithRetryIf(ctx, once, forwardRetries, retryBackoff, func(error) bool { return true })
	}
	// Non-idempotent: replay only when the error proves nothing was delivered.
	return doWithRetryIf(ctx, once, forwardRetries, retryBackoff, notDelivered)
}

// replaySafe reports whether re-issuing method after a transport error is safe.
// POST is excluded even though creates carry an Idempotency-Key: the node
// answers a duplicate in-progress create with 409, not the eventual 201.
func replaySafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// forwardOnce issues a single authenticated request to a node.
func (s *Server) forwardOnce(ctx context.Context, node *cluster.Node, method, path string, body []byte, ctype string, org ...string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	base, hop, err := s.nodeHop(node, hopBuffered)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	if node.Key != "" {
		req.Header.Set("Authorization", "Bearer "+node.Key)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	observability.Propagate(ctx, req.Header)
	observability.InjectTrace(ctx, req.Header)
	// The idempotency key travels in ctx because forwardOnce never sees the inbound request.
	if k := idempotencyKey(ctx); k != "" {
		req.Header.Set("Idempotency-Key", k)
	}
	if len(org) > 0 {
		if err := s.setOrgAndKey(ctx, req, node, method, path, org[0]); err != nil {
			return nil, err
		}
	}
	// time.Now, not s.now: the test clock jumps by design and would fake latencies.
	started := time.Now()
	resp, err := hop.Do(req)
	s.metrics.recordForward(node.ID, time.Since(started).Seconds(), err)
	return resp, err
}

// selfPreview swaps the host of a node's path-form preview URL for the orchestrator's own.
func (s *Server) selfPreview(r *http.Request, nodeURL string) string {
	if i := strings.Index(nodeURL, "/preview/"); i >= 0 {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		return fmt.Sprintf("%s://%s%s", scheme, r.Host, nodeURL[i:])
	}
	return nodeURL
}

// previewLinks turns the preview URL a node returned into the pair a caller
// can actually use: url/app_url with no credential on them, and viewer_url
// carrying the browser-hop ?token= this tier verifies — the same split the
// portal makes, and the field the refusal page tells people to open.
//
// The node's own ?ptok= never travels to a caller. It is the control-plane-to-
// node hop's credential, and a fleet node verifies it with the org-assert
// public key it cannot mint against, so relaying it hands over a link that
// cannot open. viewer_url is minted here, by the tier that checks it.
func (s *Server) previewLinks(r *http.Request, nodeURL string) (bare, viewer string) {
	bare = s.selfPreview(r, nodeURL)
	u, err := url.Parse(bare)
	if err != nil {
		return bare, ""
	}
	u.RawQuery = ""
	bare = u.String()
	id, port, ok := proxy.ParseSandboxHost(strings.ToLower(u.Hostname()), strings.ToLower(s.sandboxDomain))
	if !ok {
		id, port, _, ok = proxy.ParsePreviewPath(u.Path)
	}
	if !ok {
		return bare, ""
	}
	tok := s.previewToken(r.Context(), id, port)
	if tok == "" {
		return bare, ""
	}
	return bare, bare + "?token=" + tok
}

// rewriteURL swaps the preview URL (app_url and legacy url), publishes the
// viewer_url that opens it, and adds a node field to the create response.
// Every other node-minted link in the body goes through sanitizeNodeURLs.
func (s *Server) rewriteURL(r *http.Request, body []byte, from, to, viewer, nodeID string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if from != "" {
		if _, ok := m["app_url"]; ok {
			m["app_url"] = to
		}
		m["url"] = to
		if viewer != "" {
			m["viewer_url"] = viewer
		}
	}
	s.sanitizeNodeURLs(r, m)
	m["node"] = nodeID
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// sanitizeNodeURLs fixes every OTHER link a node minted for one of its own
// ports — terminal_url, desktop_url, whatever a later change adds. The node
// stamps its own ?ptok= on all of them (internal/httpapi appURL), exactly as it
// does on app_url, and rewriteURL used to name three fields and relay the rest
// verbatim. That relayed a credential the invariant above forbids, on a host a
// caller cannot open, so the field was advertised and useless.
//
// The ?ptok= is dropped and the link comes back pointing at this tier, carrying
// the browser token this tier verifies — viewer_url's treatment, because these
// fields name a port that has no viewer_url sibling and a bare link to a
// private sandbox opens nothing.
//
// The credential is not what makes a link the node's, though, and gating on it
// alone missed the case that matters: a node that mints no preview token stamps
// its own host on desktop_url and terminal_url all the same, and those were
// relayed untouched. So a path-form preview link counts too — its host is the
// node's, which on a real fleet is not a host the caller can open. Anything
// that is neither is left alone: it was not minted by the node's preview
// builder and is none of this function's business.
func (s *Server) sanitizeNodeURLs(r *http.Request, m map[string]any) {
	for k, v := range m {
		switch k {
		case "url", "app_url", "viewer_url": // handled above, with the node's own `from`
			continue
		}
		if !strings.HasSuffix(k, "_url") {
			continue
		}
		raw, ok := v.(string)
		if !ok || (!strings.Contains(raw, "ptok=") && !strings.Contains(raw, "/preview/")) {
			continue
		}
		bare, viewer := s.previewLinks(r, raw)
		if viewer != "" {
			m[k] = viewer
			continue
		}
		m[k] = bare
	}
}

// injectJSONFields merges extra top-level fields into a JSON object body.
func injectJSONFields(rb []byte, extra map[string]any) []byte {
	var m map[string]any
	if json.Unmarshal(rb, &m) != nil {
		return rb
	}
	maps.Copy(m, extra)
	out, err := json.Marshal(m)
	if err != nil {
		return rb
	}
	return out
}

func relay(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		if hopByHop(k, resp.Header) {
			// Relaying Transfer-Encoding would contradict Go's own framing.
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Flush streams per chunk so frames are not buffered until handler return.
	if f, ok := w.(http.Flusher); ok && isStreamingResp(resp) {
		_, _ = io.Copy(flushWriter{w, f}, resp.Body) //nolint:errcheck // best-effort: status already committed; a mid-stream error just means the client or upstream went away
		return
	}
	_, _ = io.Copy(w, resp.Body) //nolint:errcheck // best-effort: status already committed; a mid-stream error just means the client or upstream went away
}

// hopByHopHeaders is the RFC 7230 set a proxy must consume rather than forward.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"proxy-connection":    true, // non-standard but widely sent
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// hopByHop reports whether header k must not be relayed, including headers the
// peer nominated in its own Connection header.
func hopByHop(k string, h http.Header) bool {
	if hopByHopHeaders[strings.ToLower(k)] {
		return true
	}
	for _, nominated := range h.Values("Connection") {
		for _, name := range strings.Split(nominated, ",") {
			if strings.EqualFold(strings.TrimSpace(name), k) {
				return true
			}
		}
	}
	return false
}

// isStreamingResp reports whether resp is SSE or has no fixed length.
func isStreamingResp(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.ContentLength < 0
}

// flushWriter flushes after every write.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// nodeWentQuiet reports whether node has stopped heartbeating, judged by the
// registry's current record, never by the caller's copy: a drain holds its
// node for minutes, and its copy's LastSeen aged past the TTL mid-drain, which
// refused every evict and left each moved sandbox running on both nodes.
func (s *Server) nodeWentQuiet(node *cluster.Node) (time.Time, bool) {
	if node == nil {
		return time.Time{}, false
	}
	seen := node.LastSeen
	if s.reg == nil {
		return seen, !seen.IsZero() && s.now().Sub(seen) > cluster.NodeTTL
	}
	for _, n := range s.reg.Nodes() {
		if n.ID == node.ID {
			seen = n.LastSeen
			break
		}
	}
	if seen.IsZero() {
		return seen, false
	}
	return seen, s.now().Sub(seen) > cluster.NodeTTL
}
