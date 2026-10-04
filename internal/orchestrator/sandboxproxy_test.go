package orchestrator

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeNode registers an httptest server as the node owning sandbox "sb1" and
// returns the orchestrator in front of it.
func fakeNode(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	node := httptest.NewServer(h)
	t.Cleanup(node.Close)

	s := fleetServer("apikey", "nodetoken")
	orch := httptest.NewServer(s.Routes())
	t.Cleanup(orch.Close)

	registerNode(t, orch, `{"id":"n1","url":"`+node.URL+`","zone":"z1",`+
		`"capacity":{"cpu_cores":4,"mem_total_mb":8000,"mem_available_mb":4000}}`)
	s.reg.Place("sb1", "n1")
	return orch
}

func agentGET(t *testing.T, orch *httptest.Server, path string, hdr http.Header) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", orch.URL+path, nil)
	req.Header.Set("Authorization", "Bearer apikey")
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	// No redirect following, so a relayed 303 can be inspected.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func TestSandboxProxyRelaysTheResumeCursorToTheNode(t *testing.T) {
	var got http.Header
	orch := fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})

	h := http.Header{}
	h.Set("Last-Event-ID", "17")
	h.Set("Idempotency-Key", "idem-9")
	resp := agentGET(t, orch, "/v1/sandboxes/sb1/agent/events", h)
	defer resp.Body.Close()

	if got.Get("Last-Event-ID") != "17" {
		t.Errorf("Last-Event-ID = %q, want 17 — a reconnect would silently skip events",
			got.Get("Last-Event-ID"))
	}
	if got.Get("Idempotency-Key") != "idem-9" {
		t.Errorf("Idempotency-Key dropped: a retried turn submission would run twice")
	}
	// Nodes run customer code; the orchestrator key must not reach one.
	if a := got.Get("Authorization"); a == "Bearer apikey" {
		t.Error("the orchestrator forwarded the client's own API key to the node")
	}
	if !strings.HasPrefix(got.Get("Authorization"), "Bearer sf_node_") {
		t.Errorf("node saw Authorization %q, want its derived node key", got.Get("Authorization"))
	}
}

func TestSandboxProxyDoesNotRelayCookiesToNodes(t *testing.T) {
	var got http.Header
	orch := fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})

	h := http.Header{}
	h.Set("Cookie", "sandboxd_session=portal-secret")
	resp := agentGET(t, orch, "/v1/sandboxes/sb1/agent/status", h)
	defer resp.Body.Close()

	if c := got.Get("Cookie"); c != "" {
		t.Fatalf("the caller's cookies reached the node: %q", c)
	}
}

// http.Client.Timeout includes reading the body, so any value caps how long a stream may be watched.
func TestSandboxProxyStreamsEventsPastTheOldTimeout(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	if s.stream.Timeout != 0 {
		t.Fatalf("stream client Timeout = %v; an event stream dies after that long",
			s.stream.Timeout)
	}
	if s.client.Timeout == 0 {
		t.Fatal("the non-streaming client lost its timeout; a hung node would hang the API")
	}
	tr, ok := s.stream.Transport.(*http.Transport)
	if !ok {
		t.Fatal("stream client has no *http.Transport to carry the narrower bounds")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Fatal("no ResponseHeaderTimeout: a node that connects and then says nothing " +
			"would hold the request open forever")
	}
}

// Reads the stream while the node still holds it open, so a buffering relay fails.
func TestSandboxProxyDeliversFramesAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	orch := fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprint(w, "id: 1\nevent: turn.started\ndata: {}\n\n")
		fl.Flush()
		<-release
		fmt.Fprint(w, "id: 2\nevent: turn.completed\ndata: {}\n\n")
		fl.Flush()
	})

	resp := agentGET(t, orch, "/v1/sandboxes/sb1/agent/events", nil)
	defer resp.Body.Close()
	defer close(release)

	first := make(chan string, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if strings.HasPrefix(line, "event:") {
				first <- strings.TrimSpace(line)
				return
			}
			if err != nil {
				first <- "read error: " + err.Error()
				return
			}
		}
	}()

	select {
	case got := <-first:
		if got != "event: turn.started" {
			t.Fatalf("first frame = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame within 3s while the node still holds the stream open — the " +
			"control plane is buffering, so the timeline arrives only after the turn ends")
	}
}

// A followed 303 would drop the claim's Set-Cookie after the single-use code was spent.
func TestSandboxProxyRelaysARedirectInsteadOfFollowingIt(t *testing.T) {
	var hits int
	orch := fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.Contains(r.URL.Path, "/agent/claim") {
			http.SetCookie(w, &http.Cookie{Name: "sandboxd_agent", Value: "sfa_x_y",
				Path: "/v1/sandboxes/sb1/agent", HttpOnly: true})
			http.Redirect(w, r, "/sandboxes/sb1?view=agent", http.StatusSeeOther)
			return
		}
		_, _ = w.Write([]byte("<html>the SPA</html>"))
	})

	resp := agentGET(t, orch, "/v1/sandboxes/sb1/agent/claim?code=abc", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want the 303 relayed", resp.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("node hit %d times; the redirect was followed", hits)
	}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == "sandboxd_agent" {
			found = true
		}
	}
	if !found {
		t.Fatal("Set-Cookie was not relayed; the claim would grant nothing")
	}
}

// The spend cap is a PATCH; tightening the catch-all to GET/POST/DELETE would break it.
func TestSandboxProxyProxiesEveryMethod(t *testing.T) {
	var seen []string
	orch := fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	})

	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/sandboxes/sb1/agent/status"},
		{"POST", "/v1/sandboxes/sb1/agent/turns"},
		{"PATCH", "/v1/sandboxes/sb1/agent/budget"},
		{"DELETE", "/v1/sandboxes/sb1/agent/tokens/tk1"},
	} {
		seen = nil
		req, _ := http.NewRequest(c.method, orch.URL+c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer apikey")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("%s %s → %d at the control plane", c.method, c.path, resp.StatusCode)
			continue
		}
		if len(seen) != 1 || seen[0] != c.method+" "+c.path {
			t.Errorf("%s %s reached the node as %v", c.method, c.path, seen)
		}
	}
}

func TestSandboxProxyQueryStringSurvivesTheHop(t *testing.T) {
	var rawQuery string
	orch := fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	})

	resp := agentGET(t, orch, "/v1/sandboxes/sb1/agent/events?since_seq=40&thread=main", nil)
	defer resp.Body.Close()

	if rawQuery != "since_seq=40&thread=main" {
		t.Fatalf("node saw query %q, want since_seq=40&thread=main", rawQuery)
	}
}
