// Package httpx holds the outbound HTTP transports shared by every component
// that proxies to a node or a sandbox.
package httpx

import (
	"net"
	"net/http"
	"time"
)

// Timeouts applied to every internal transport.
const (
	// DialTimeout bounds the TCP connect; without it a dropped SYN costs the OS default (~130s).
	DialTimeout = 5 * time.Second
	// ResponseHeaderTimeout bounds a silent peer on the preview fanout, where
	// the peer is a tenant's own application: one that has not begun answering
	// in 30 seconds is not going to. Not an overall client timeout, because
	// these transports carry SSE and long-lived agent streams.
	ResponseHeaderTimeout = 30 * time.Second

	// NodeSyncCeiling is the longest a node may take to produce the first byte
	// of a synchronous answer. Not a transport preference: it is the node's own
	// ceiling on foreground work, and internal/httpapi bounds a foreground exec
	// to exactly this. It lives here because the bound and the ceiling it must
	// cover cannot be allowed to drift.
	//
	// Measured on the two-node fleet before this existed: `exec "sleep 20"`
	// returned 200 in 20.4s and `exec "sleep 40"` returned 502 "timeout
	// awaiting response headers" in 30.3s — while the command ran to completion
	// on the node and its result was thrown away. The node's own policy has
	// always admitted a 600-second foreground exec; the transport capped it at
	// 30. A first-byte bound below the peer's declared ceiling is not
	// protection from a silent peer, it is a cap on the platform.
	NodeSyncCeiling = 600 * time.Second

	// nodeHeaderTimeout is the silent-peer bound on the control-plane-to-node
	// hop. Deliberately above NodeSyncCeiling so the node always wins the race:
	// it answers, or reports its own timeout, before the control plane gives
	// up. That ordering is what makes a completed command's result impossible
	// to lose — the alternative is what the fleet measured, a 502 for work that
	// succeeded.
	nodeHeaderTimeout = NodeSyncCeiling + 30*time.Second

	IdleConnTimeout       = 90 * time.Second
	TLSHandshakeTimeout   = 10 * time.Second
	keepAlive             = 30 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// NewTransport returns the transport for control-plane to node traffic.
// A literal &http.Transport{} has no dial timeout at all and pools only 2
// idle connections per host; both matter for a process that only proxies.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   DialTimeout,
			KeepAlive: keepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       IdleConnTimeout,
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: nodeHeaderTimeout,
	}
}

// NewWorkTransport returns the transport for a node call whose first byte
// legitimately arrives minutes late: an image build, or a snapshot capture that
// writes a guest's whole RAM to disk before it can answer. There is no
// first-byte bound at all, because there is no honest guess for one — a larger
// image on a slower link blows any fixed number.
//
// It is correct only where the caller sets an overall http.Client.Timeout, and
// that timeout is then the bound. Never use it for a request nothing else
// bounds: an unbounded first byte and no overall deadline is a leaked request.
func NewWorkTransport() *http.Transport {
	tr := NewTransport()
	tr.ResponseHeaderTimeout = 0
	return tr
}

// NewFanoutTransport returns the transport for the preview proxy, where every
// sandbox is its own host. The total idle pool is bounded rather than the
// per-host share, and HTTP/2 is off because WebSocket upgrades need HTTP/1.1.
func NewFanoutTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   DialTimeout,
			KeepAlive: keepAlive,
		}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       IdleConnTimeout,
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: ResponseHeaderTimeout,
	}
}
