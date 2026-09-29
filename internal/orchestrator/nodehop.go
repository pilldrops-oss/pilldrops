package orchestrator

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
)

// Every request on the orchestrator-to-node hop carries that node's operator
// credential, so the hop is protected by pinning the node's public key
// (learned on an authenticated heartbeat) and checking it on every dial.

// errNodeHopInsecure is returned when enforcement is on and the node has no verified channel.
var errNodeHopInsecure = errors.New("refusing to forward over an unprotected hop")

// hopMode says what shape of call a hop carries, which is what picks the
// transport. The three differ only in what bounds them, and getting that wrong
// is how a working node gets reported as a broken one.
type hopMode int

const (
	// hopBuffered is an ordinary request bounded by the client's overall Timeout.
	hopBuffered hopMode = iota
	// hopStream carries SSE and WebSocket upgrades, so it can have no overall
	// Timeout and is bounded by the transport's first-byte guard alone.
	hopStream
	// hopWork carries a call whose first byte legitimately arrives minutes
	// late -- an image build, a snapshot capture -- and is bounded only by the
	// caller's own deadline.
	hopWork
)

// nodeHop returns the base URL and client for reaching node. The scheme comes
// from the pin, not the registered URL: a node serves TLS and plaintext on the
// same port, so a pinned node's http:// URL is upgraded to https:// in place.
func (s *Server) nodeHop(node *cluster.Node, mode hopMode) (base string, client *http.Client, err error) {
	base = strings.TrimRight(node.URL, "/")
	plain := s.client
	switch mode {
	case hopStream:
		if s.stream != nil {
			// A Server built literally in a test has no stream client.
			plain = s.stream
		}
	case hopWork:
		plain = s.workClient()
	}

	if node.TLSSPKI == "" {
		if nodetls.RequireSecureHop() {
			// An https:// URL without a pin is not a verified hop; the pin is the binding.
			return "", nil, fmt.Errorf("%w: node %s advertises no TLS pin and %s=1 is set "+
				"(upgrade the node, or unset %s until the fleet has rolled)",
				errNodeHopInsecure, node.ID, nodetls.RequireSecureHopEnv, nodetls.RequireSecureHopEnv)
		}
		return base, plain, nil
	}

	if rest, ok := strings.CutPrefix(base, "http://"); ok {
		base = "https://" + rest
	}
	c, err := s.pinnedClient(node.TLSSPKI, mode)
	if err != nil {
		return "", nil, err
	}
	return base, c, nil
}

// pinnedClient returns the cached http.Client for nodes holding spki.
// Keyed by pin rather than node id so a re-keyed node gets a fresh transport,
// and by mode so one pin has a client per bound.
func (s *Server) pinnedClient(spki string, mode hopMode) (*http.Client, error) {
	key := hopKeys[mode] + spki
	if c, ok := s.hopClients.Load(key); ok {
		return c.(*http.Client), nil
	}
	cfg, err := nodetls.PinnedClientConfig(spki)
	if err != nil {
		return nil, err
	}
	tr := httpx.NewTransport()
	if mode == hopWork {
		tr = httpx.NewWorkTransport()
	}
	tr.TLSClientConfig = cfg
	// With a custom TLSClientConfig Go stays on HTTP/1.1, which the WebSocket
	// upgrades on the streamed paths need.
	c := &http.Client{Transport: tr}
	if mode == hopBuffered {
		c.Timeout = nodeClientTimeout
	} else {
		// Mirror newStreamClient: no overall timeout, redirects relayed not
		// followed. A hopWork caller replaces this client with one carrying its
		// own deadline, which is what bounds it.
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	actual, _ := s.hopClients.LoadOrStore(key, c)
	return actual.(*http.Client), nil
}

// hopKeys namespaces the client cache by mode.
var hopKeys = map[hopMode]string{hopBuffered: "", hopStream: "stream|", hopWork: "work|"}

// workClient is the unpinned client for hopWork. Cached in hopClients so a
// Server built literally in a test needs no extra field.
func (s *Server) workClient() *http.Client {
	const key = "work|plain"
	if c, ok := s.hopClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := &http.Client{
		Transport:     httpx.NewWorkTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	actual, _ := s.hopClients.LoadOrStore(key, c)
	return actual.(*http.Client)
}

// nodeHopLong is nodeHop with its own overall deadline, for a call whose first
// byte legitimately arrives minutes late: an image build, or a snapshot capture
// that writes a guest's whole RAM before it can answer.
//
// It used to borrow the streaming client's transport, which carries a first-byte
// bound -- so every one of these was capped at that bound however generous a
// timeout was passed here, and the claim that they "outlast the ordinary node
// timeout" was not true of the bound that actually fired. hopWork has no
// first-byte bound, and timeout is the whole of what bounds the call.
func (s *Server) nodeHopLong(node *cluster.Node, timeout time.Duration) (string, *http.Client, error) {
	base, c, err := s.nodeHop(node, hopWork)
	if err != nil {
		return "", nil, err
	}
	return base, &http.Client{Transport: c.Transport, Timeout: timeout}, nil
}

// nodeClientTimeout mirrors s.client's timeout as a constant so a test-built
// Server cannot leak its zero value into the pinned path.
const nodeClientTimeout = 150 * time.Second

// hopClientCache is a sync.Map so the zero value on a literally-built Server is usable.

type hopClientCache = sync.Map
