package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNodeTransportOutlastsTheNodesOwnCeiling is the check for the 30-second
// cap on every synchronous node call. The node admits a foreground exec of up
// to NodeSyncCeiling, so a first-byte bound below that reports work the node is
// still doing -- and will still finish -- as a transport failure. Measured on
// the two-node fleet: `exec "sleep 40"` gave 502 "timeout awaiting response
// headers" at 30.3s while the command completed on the node and its result was
// discarded.
//
// The ordering is the guarantee, not the numbers: while the node's ceiling
// fires first, a completed command's result cannot be lost.
func TestNodeTransportOutlastsTheNodesOwnCeiling(t *testing.T) {
	got := NewTransport().ResponseHeaderTimeout
	if got <= NodeSyncCeiling {
		t.Fatalf("the node transport gives up on a first byte after %s but a node may work for "+
			"%s before answering — every operation between the two returns 502 for work that "+
			"succeeded", got, NodeSyncCeiling)
	}
}

// TestFanoutKeepsTheSilentPeerBound guards the other half: the preview fanout
// talks to a tenant's own application, where 30 seconds of silence really is a
// failure, and it must not be widened along with the node hop.
func TestFanoutKeepsTheSilentPeerBound(t *testing.T) {
	if got := NewFanoutTransport().ResponseHeaderTimeout; got != ResponseHeaderTimeout {
		t.Fatalf("the preview fanout's first-byte bound is %s, want %s", got, ResponseHeaderTimeout)
	}
}

// TestWorkTransportHasNoFirstByteBound proves the two transports actually
// differ in the way the callers rely on: a work call's first byte may arrive
// arbitrarily late and only the caller's own deadline bounds it.
func TestWorkTransportHasNoFirstByteBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond) // the node is working, not silent
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A transport that does bound the first byte gives up.
	bounded := NewTransport()
	bounded.ResponseHeaderTimeout = 20 * time.Millisecond
	if _, err := (&http.Client{Transport: bounded}).Get(srv.URL); err == nil {
		t.Fatal("a first-byte bound shorter than the peer's work did not fire; the rest of this " +
			"test proves nothing")
	}

	work := NewWorkTransport()
	if work.ResponseHeaderTimeout != 0 {
		t.Fatalf("the work transport bounds the first byte at %s; it must not bound it at all",
			work.ResponseHeaderTimeout)
	}
	resp, err := (&http.Client{Transport: work, Timeout: 5 * time.Second}).Get(srv.URL)
	if err != nil {
		t.Fatalf("the work transport gave up on a peer that was working: %v", err)
	}
	_ = resp.Body.Close()
}
