package main

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamToOrchestratorRelaysAllowlistedHeaders: relayHeaders is an
// allowlist a caller opts extra headers into (e.g. Last-Event-ID for a
// resumable stream); the inbound Authorization must never survive the hop.
func TestStreamToOrchestratorRelaysAllowlistedHeaders(t *testing.T) {
	var got http.Header
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	defer orch.Close()

	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/exec/stream", nil)
	req.Header.Set("Last-Event-ID", "41")
	req.Header.Set("Idempotency-Key", "idem-1")
	// The customer's own credential must not travel onward.
	req.Header.Set("Authorization", "Bearer sf_live_customer")

	rr := httptest.NewRecorder()
	streamToOrchestrator(rr, req, orch.URL, "orch-key", "u:alice", "Last-Event-ID", "Idempotency-Key")

	if got.Get("Last-Event-ID") != "41" {
		t.Errorf("Last-Event-ID = %q, want 41 — a reconnect would silently skip events",
			got.Get("Last-Event-ID"))
	}
	if got.Get("Idempotency-Key") != "idem-1" {
		t.Errorf("Idempotency-Key dropped; a retried request would run twice")
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

// TestStreamToOrchestratorRelaysSetCookieAndDoesNotFollowRedirects: a client
// that followed the redirect itself would report the target's status instead
// of the one the node actually answered with.
func TestStreamToOrchestratorRelaysSetCookieAndDoesNotFollowRedirects(t *testing.T) {
	var hits int
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.SetCookie(w, &http.Cookie{Name: "sandboxd_session", Value: "sfs_abc_secret",
			Path: "/", HttpOnly: true})
		w.Header().Set("Location", "/sandboxes/sb1")
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer orch.Close()

	rr := httptest.NewRecorder()
	streamToOrchestrator(rr, httptest.NewRequest("GET", "/v1/sandboxes/sb1/exec/stream?code=xyz", nil),
		orch.URL, "k", "u:alice")

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 relayed (a followed redirect would report the target's status)", rr.Code)
	}
	if hits != 1 {
		t.Fatalf("orchestrator hit %d times; a single-use code must be spent once", hits)
	}
	if c := rr.Header().Get("Set-Cookie"); !strings.Contains(c, "sandboxd_session=") {
		t.Fatalf("Set-Cookie not relayed (%q)", c)
	}
	if rr.Header().Get("Location") != "/sandboxes/sb1" {
		t.Errorf("Location not relayed: %q", rr.Header().Get("Location"))
	}
}

// TestStreamToOrchestratorArrivesIncrementally reads the response while
// upstream is still writing; a buffered proxy would yield the same bytes,
// only all at once.
func TestStreamToOrchestratorArrivesIncrementally(t *testing.T) {
	release := make(chan struct{})
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "id: 1\nevent: started\ndata: {}\n\n")
		fl.Flush()
		<-release // hold the stream open: a buffering hop cannot get past this
		fmt.Fprint(w, "id: 2\nevent: completed\ndata: {}\n\n")
		fl.Flush()
	}))
	defer orch.Close()

	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamToOrchestrator(w, r, orch.URL, "k", "u:alice")
	}))
	defer portal.Close()

	resp, err := http.Get(portal.URL + "/v1/sandboxes/sb1/exec/stream")
	if err != nil {
		t.Fatalf("GET exec/stream: %v", err)
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
		if got != "event: started" {
			t.Fatalf("first frame = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame within 3s while the stream is still open — the portal is buffering, " +
			"so the timeline would appear all at once after the run finished")
	}
}

// TestStreamToOrchestratorCarriesNoContentLength: a relayed Content-Length
// contradicts a chunked body and some clients wait for the promised bytes.
func TestStreamToOrchestratorCarriesNoContentLength(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "12")
		_, _ = w.Write([]byte("data: hello\n"))
	}))
	defer orch.Close()

	rr := httptest.NewRecorder()
	streamToOrchestrator(rr, httptest.NewRequest("GET", "/v1/sandboxes/sb1/exec/stream", nil),
		orch.URL, "k", "u:alice")

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
		t.Fatal("redirects are followed; a claim flow's Set-Cookie would be discarded")
	}
}
