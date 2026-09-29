package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
)

// These tests go through a real TLS listener and assert on what the node
// observed (r.TLS), not on the URL the orchestrator composed.

// tlsNode starts a TLS node with its own identity and records whether each request arrived encrypted.
func tlsNode(t *testing.T) (id *nodetls.Identity, srv *httptest.Server, sawTLS *[]bool) {
	t.Helper()
	id, err := nodetls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var seen []bool
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.TLS != nil)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.TLS = id.ServerConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return id, srv, &seen
}

// hopServer is a Server wired like NewServer without the rest of the control plane.
func hopServer() *Server {
	return &Server{
		client:  &http.Client{Timeout: 10 * time.Second, Transport: httpx.NewTransport()},
		stream:  newStreamClient(),
		metrics: newOrchMetrics(),
	}
}

func TestForwardToAPinnedNodeIsEncryptedAndVerified(t *testing.T) {
	id, srv, sawTLS := tlsNode(t)
	s := hopServer()

	// The registered URL is http://; the scheme upgrade must come from the pin.
	plainURL := "http://" + strings.TrimPrefix(srv.URL, "https://")
	node := &cluster.Node{ID: "node-a", URL: plainURL, Key: "k", TLSSPKI: id.SPKI}

	resp, err := s.forward(t.Context(), node, http.MethodGet, "/v1/sandboxes", nil, "")
	if err != nil {
		t.Fatalf("forward to a pinned node failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(*sawTLS) != 1 || !(*sawTLS)[0] {
		t.Fatalf("the node saw r.TLS = %v — the forward went out in cleartext despite the pin", *sawTLS)
	}
}

func TestForwardToANodeServingTheWrongKeyIsRefused(t *testing.T) {
	_, srv, _ := tlsNode(t)
	imposter, err := nodetls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := hopServer()
	node := &cluster.Node{ID: "node-a", URL: srv.URL, Key: "k", TLSSPKI: imposter.SPKI}

	_, err = s.forward(t.Context(), node, http.MethodGet, "/v1/sandboxes", nil, "")
	if err == nil {
		t.Fatal("a forward completed against a node presenting a key the control plane never pinned")
	}
	if !strings.Contains(err.Error(), nodetls.ErrPinMismatch.Error()) {
		t.Errorf("failed for the wrong reason: %v\nwant a pin mismatch — anything else means this "+
			"test proved unreachability, not verification", err)
	}
}

func TestAnUnpinnedNodeStillWorksUntilEnforcementIsOn(t *testing.T) {
	var sawTLS []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTLS = append(sawTLS, r.TLS != nil)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s := hopServer()
	node := &cluster.Node{ID: "old-node", URL: srv.URL, Key: "k"} // no pin

	t.Setenv(nodetls.RequireSecureHopEnv, "")
	resp, err := s.forward(t.Context(), node, http.MethodGet, "/v1/sandboxes", nil, "")
	if err != nil {
		t.Fatalf("a pre-upgrade node was refused with enforcement OFF: %v", err)
	}
	resp.Body.Close()
	if len(sawTLS) != 1 || sawTLS[0] {
		t.Errorf("sawTLS = %v, want one plaintext request", sawTLS)
	}

	t.Setenv(nodetls.RequireSecureHopEnv, "1")
	if _, err := s.forward(t.Context(), node, http.MethodGet, "/v1/sandboxes", nil, ""); err == nil {
		t.Fatal("an unpinned node was still forwarded to with enforcement ON")
	} else if !strings.Contains(err.Error(), "advertises no TLS pin") {
		t.Errorf("refusal did not name the reason: %v", err)
	}
	if len(sawTLS) != 1 {
		t.Errorf("the node received %d requests, want 1 — the refused forward still went out", len(sawTLS))
	}
}

func TestEveryNodeDialGoesThroughTheHop(t *testing.T) {
	id, srv, sawTLS := tlsNode(t)
	s := hopServer()
	plainURL := "http://" + strings.TrimPrefix(srv.URL, "https://")
	node := &cluster.Node{ID: "node-a", URL: plainURL, Key: "k", TLSSPKI: id.SPKI}

	// nodeHopLong is what the image build uses.
	base, long, err := s.nodeHopLong(node, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(base, "https://") {
		t.Errorf("nodeHopLong base = %q, want https", base)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", base+"/v1/images/export", nil)
	resp, err := long.Do(req)
	if err != nil {
		t.Fatalf("long hop: %v", err)
	}
	resp.Body.Close()

	// forwardStream is what the sandbox proxy and the image export use.
	sresp, err := s.forwardStream(t.Context(), node, "GET", "/v1/sandboxes/x/logs", nil, "", http.Header{})
	if err != nil {
		t.Fatalf("stream hop: %v", err)
	}
	sresp.Body.Close()

	for i, tlsSeen := range *sawTLS {
		if !tlsSeen {
			t.Errorf("request %d reached the node in cleartext", i)
		}
	}
	if len(*sawTLS) != 2 {
		t.Errorf("node saw %d requests, want 2", len(*sawTLS))
	}
}

func TestPinnedClientsAreCachedPerPin(t *testing.T) {
	s := hopServer()
	a, err := s.pinnedClient("pin-a", hopBuffered)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.pinnedClient("pin-a", hopBuffered)
	if err != nil {
		t.Fatal(err)
	}
	if a != again {
		t.Error("a second call for the same pin built a second client")
	}
	b, err := s.pinnedClient("pin-b", hopBuffered)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two different pins share one client — one of them is verifying the wrong key")
	}
	stream, err := s.pinnedClient("pin-a", hopStream)
	if err != nil {
		t.Fatal(err)
	}
	if stream == a {
		t.Error("the streaming and buffered clients for one pin are the same object; " +
			"the streaming one must have no overall timeout")
	}
	if stream.Timeout != 0 {
		t.Errorf("streaming client Timeout = %v, want 0 — an overall deadline severs SSE mid-frame", stream.Timeout)
	}
}

// Refusal happens at registration so the scheduler never places on an unpinned node.
func TestHeartbeatRefusesAnUnpinnedNodeWhenEnforcing(t *testing.T) {
	s := NewServer("k", "t")
	s.nodeKeySecret = "node-key-secret"

	// The bearer is per-node, so derive it from the id in the body.
	post := func(body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/internal/heartbeat", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+s.nodeHeartbeatToken(body["id"].(string), "default"))
		w := httptest.NewRecorder()
		s.handleHeartbeat(w, req)
		return w
	}

	t.Setenv(nodetls.RequireSecureHopEnv, "1")
	if w := post(map[string]any{"id": "node-a", "url": "http://a:18080"}); w.Code != 426 {
		t.Errorf("unpinned heartbeat with enforcement on = %d, want 426; body=%s", w.Code, w.Body.String())
	}
	// Positive control: with a pin it registers.
	if w := post(map[string]any{
		"id": "node-a", "url": "http://a:18080", "tls_spki": "some-pin", "ts": time.Now().UnixMilli(),
	}); w.Code != 200 {
		t.Fatalf("a pinned heartbeat was refused: %d %s", w.Code, w.Body.String())
	}
	t.Setenv(nodetls.RequireSecureHopEnv, "")
	if w := post(map[string]any{"id": "node-b", "url": "http://b:18080"}); w.Code != 200 {
		t.Errorf("unpinned heartbeat with enforcement off = %d, want 200", w.Code)
	}
}

func TestHeartbeatHandlerRefusesAReplay(t *testing.T) {
	s := NewServer("k", "t")
	s.nodeKeySecret = "node-key-secret"
	now := time.Now()
	body := map[string]any{"id": "node-a", "url": "http://a:18080", "ts": now.UnixMilli()}
	b, _ := json.Marshal(body)

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/internal/heartbeat", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+s.nodeHeartbeatToken("node-a", "default"))
		w := httptest.NewRecorder()
		s.handleHeartbeat(w, req)
		return w
	}
	if w := post(); w.Code != 200 {
		t.Fatalf("first beat = %d %s", w.Code, w.Body.String())
	}
	w := post()
	if w.Code != 409 {
		t.Errorf("replayed beat = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stale heartbeat") {
		t.Errorf("refusal does not name the reason: %s", w.Body.String())
	}
}
