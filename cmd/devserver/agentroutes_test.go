package main

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// mountAgentAPI mirrors the production wiring: the middleware has the same shape
// as main()'s sandboxProxy and calls the same serveAgentProxy, because the
// ordering between middleware and route body is what is under test.
func mountAgentAPI(proxyMode bool, orchURL, orchKey string) *chi.Mux {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if !proxyMode || !isFleetPath(req.URL.Path) {
					next.ServeHTTP(w, req)
					return
				}
				if isAgentPath(req.URL.Path) {
					serveAgentProxy(w, req, orchURL, orchKey, "u:alice")
					return
				}
				next.ServeHTTP(w, req)
			})
		})
		registerAgentRoutes(r, proxyMode)
	})
	return r
}

// TestPortalRoutesTheWholeAgentSubtree: chi runs middleware only for matched
// routes, so every agent path must match or it 404s at the portal.
func TestPortalRoutesTheWholeAgentSubtree(t *testing.T) {
	var seen []string
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer orch.Close()

	r := mountAgentAPI(true, orch.URL, "orch-key")

	// The node's full agent surface, method included.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/sandboxes/sb1/agent/status"},
		{"POST", "/v1/sandboxes/sb1/agent/turns"},
		{"GET", "/v1/sandboxes/sb1/agent/turns"},
		{"GET", "/v1/sandboxes/sb1/agent/turns/t1"},
		{"DELETE", "/v1/sandboxes/sb1/agent/turns/t1"},
		{"POST", "/v1/sandboxes/sb1/agent/turns/t1/interrupt"},
		{"POST", "/v1/sandboxes/sb1/agent/queue/reorder"},
		{"GET", "/v1/sandboxes/sb1/agent/events"},
		{"GET", "/v1/sandboxes/sb1/agent/models"},
		{"GET", "/v1/sandboxes/sb1/agent/budget"},
		{"PATCH", "/v1/sandboxes/sb1/agent/budget"},
		{"GET", "/v1/sandboxes/sb1/agent/threads"},
		{"GET", "/v1/sandboxes/sb1/agent/threads/th1"},
		{"POST", "/v1/sandboxes/sb1/agent/tokens"},
		{"GET", "/v1/sandboxes/sb1/agent/tokens"},
		{"DELETE", "/v1/sandboxes/sb1/agent/tokens/tk1"},
		{"POST", "/v1/sandboxes/sb1/agent/tokens/tk1/rotate"},
		{"POST", "/v1/sandboxes/sb1/agent/share"},
		{"POST", "/v1/sandboxes/sb1/agent/responses"},
		{"GET", "/v1/sandboxes/sb1/agent/responses/r1"},
		{"GET", "/v1/sandboxes/sb1/agent/responses/r1/stream"},
		{"POST", "/v1/sandboxes/sb1/agent/responses/r1/cancel"},
	} {
		seen = nil
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, strings.NewReader("{}")))
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s %s → 404 at the portal; the node serves it", c.method, c.path)
			continue
		}
		if rr.Code != http.StatusOK {
			t.Errorf("%s %s → %d, want 200", c.method, c.path, rr.Code)
			continue
		}
		// A 200 produced by the portal itself would be a different bug.
		if len(seen) != 1 || seen[0] != c.method+" "+c.path {
			t.Errorf("%s %s reached the orchestrator as %v", c.method, c.path, seen)
		}
	}
}

// TestBudgetPatchIsNotSwallowed: fleetPassthrough registers GET, POST and DELETE
// only, so PATCH must reach the orchestrator through the agent subtree.
func TestBudgetPatchIsNotSwallowed(t *testing.T) {
	var method string
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_, _ = w.Write([]byte(`{}`))
	}))
	defer orch.Close()

	rr := httptest.NewRecorder()
	mountAgentAPI(true, orch.URL, "k").ServeHTTP(rr,
		httptest.NewRequest("PATCH", "/v1/sandboxes/sb1/agent/budget", strings.NewReader(`{"limit_usd":5}`)))

	if rr.Code == http.StatusMethodNotAllowed {
		t.Fatal("PATCH /agent/budget → 405: the spend cap cannot be set from the dashboard")
	}
	if method != "PATCH" {
		t.Fatalf("orchestrator saw method %q, want PATCH", method)
	}
}

// TestAgentPathIsSegmentMatched: a substring match would misfire on a sandbox
// id that contains "agent".
func TestAgentPathIsSegmentMatched(t *testing.T) {
	for _, p := range []string{
		"/v1/sandboxes/sb1/agent/status",
		"/v1/sandboxes/agent-1/agent/events",
	} {
		if !isAgentPath(p) {
			t.Errorf("isAgentPath(%q) = false", p)
		}
	}
	for _, p := range []string{
		"/v1/sandboxes/my-agent-box/exec",    // id contains "agent"
		"/v1/sandboxes/sb1/agentless",        // not a segment
		"/v1/sandboxes",                      // the collection
		"/v1/cron",                           // a different resource
		"/agent/claim",                       // not under a sandbox
		"/v1/sandboxes/sb1/files?p=/agent/x", // only in the query
	} {
		if isAgentPath(p) {
			t.Errorf("isAgentPath(%q) = true", p)
		}
	}
}

// TestAgentAPIWithoutFleetSaysWhy: no orchestrator must answer 501 naming the
// variables to set, not 404.
func TestAgentAPIWithoutFleetSaysWhy(t *testing.T) {
	rr := httptest.NewRecorder()
	mountAgentAPI(false, "", "").ServeHTTP(rr,
		httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent/status", nil))

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"PORTAL_ORCHESTRATOR_URL", "agentd"} {
		if !strings.Contains(body, want) {
			t.Errorf("501 body does not mention %s: %s", want, body)
		}
	}
}

// TestAgentProxyRelaysTheResumeCursor: Last-Event-ID is the journal cursor
// EventSource sends on reconnect; dropping it silently loses events.
func TestAgentProxyRelaysTheResumeCursor(t *testing.T) {
	var got http.Header
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	defer orch.Close()

	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent/events", nil)
	req.Header.Set("Last-Event-ID", "41")
	req.Header.Set("Idempotency-Key", "idem-1")
	// The customer's own credential must not travel onward.
	req.Header.Set("Authorization", "Bearer sf_live_customer")

	rr := httptest.NewRecorder()
	mountAgentAPI(true, orch.URL, "orch-key").ServeHTTP(rr, req)

	if got.Get("Last-Event-ID") != "41" {
		t.Errorf("Last-Event-ID = %q, want 41 — a reconnect would silently skip events",
			got.Get("Last-Event-ID"))
	}
	if got.Get("Idempotency-Key") != "idem-1" {
		t.Errorf("Idempotency-Key dropped; a retried turn would queue twice")
	}
	if got.Get("Authorization") != "Bearer orch-key" {
		t.Errorf("Authorization = %q, want the orchestrator key — the customer's own "+
			"credential must not be forwarded", got.Get("Authorization"))
	}
	if got.Get("X-Org-Override") != "u:alice" {
		t.Errorf("X-Org-Override = %q, want the owner: tenancy is server-authoritative",
			got.Get("X-Org-Override"))
	}
}

// TestAgentProxyRelaysSetCookieAndDoesNotFollowRedirects: a claim answers 303 +
// Set-Cookie and the cookie is the outcome; following the redirect would discard
// it after spending a single-use code.
func TestAgentProxyRelaysSetCookieAndDoesNotFollowRedirects(t *testing.T) {
	var hits int
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.SetCookie(w, &http.Cookie{Name: "sandboxd_agent", Value: "sfa_abc_secret",
			Path: "/v1/sandboxes/sb1/agent", HttpOnly: true})
		w.Header().Set("Location", "/sandboxes/sb1?view=agent")
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer orch.Close()

	rr := httptest.NewRecorder()
	mountAgentAPI(true, orch.URL, "k").ServeHTTP(rr,
		httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent/claim?code=xyz", nil))

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 relayed (a followed redirect would report the target's status)", rr.Code)
	}
	if hits != 1 {
		t.Fatalf("orchestrator hit %d times; a single-use code must be spent once", hits)
	}
	if c := rr.Header().Get("Set-Cookie"); !strings.Contains(c, "sandboxd_agent=") {
		t.Fatalf("Set-Cookie not relayed (%q); the claim would set no credential", c)
	}
	if rr.Header().Get("Location") != "/sandboxes/sb1?view=agent" {
		t.Errorf("Location not relayed: %q", rr.Header().Get("Location"))
	}
}

// TestAgentEventStreamArrivesIncrementally reads the response while upstream is
// still writing; a buffered proxy yields the same bytes, only all at once.
func TestAgentEventStreamArrivesIncrementally(t *testing.T) {
	release := make(chan struct{})
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "id: 1\nevent: turn.started\ndata: {}\n\n")
		fl.Flush()
		<-release // hold the stream open: a buffering hop cannot get past this
		fmt.Fprint(w, "id: 2\nevent: turn.completed\ndata: {}\n\n")
		fl.Flush()
	}))
	defer orch.Close()

	portal := httptest.NewServer(mountAgentAPI(true, orch.URL, "k"))
	defer portal.Close()

	resp, err := http.Get(portal.URL + "/v1/sandboxes/sb1/agent/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	defer close(release)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

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
		t.Fatal("no frame within 3s while the stream is still open — the portal is buffering, " +
			"so the timeline would appear all at once after the turn finished")
	}
}

// TestStreamedResponseCarriesNoContentLength: a relayed Content-Length
// contradicts a chunked body and some clients wait for the promised bytes.
func TestStreamedResponseCarriesNoContentLength(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "12")
		_, _ = w.Write([]byte("data: hello\n"))
	}))
	defer orch.Close()

	rr := httptest.NewRecorder()
	mountAgentAPI(true, orch.URL, "k").ServeHTTP(rr,
		httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent/events", nil))

	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q on a streamed response", got)
	}
}

// TestPortalStreamClientHasNoOverallTimeout: http.Client.Timeout covers reading
// the body, so any non-zero value cuts an event stream mid-frame.
func TestPortalStreamClientHasNoOverallTimeout(t *testing.T) {
	c := newPortalStreamClient()
	if c.Timeout != 0 {
		t.Fatalf("Timeout = %v; an event stream would be severed after that long", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected an *http.Transport carrying the narrower bounds")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Fatal("no ResponseHeaderTimeout: a node that accepts a connection and then " +
			"says nothing would hang the request forever")
	}
	if c.CheckRedirect == nil {
		t.Fatal("redirects are followed; the claim flow's Set-Cookie would be discarded")
	}
}

// The dashboard reads the subtree's root; it must get the same explanation.
func TestAgentRootWithoutFleetSaysWhy(t *testing.T) {
	rr := httptest.NewRecorder()
	mountAgentAPI(false, "", "").ServeHTTP(rr, httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent", nil))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
}
