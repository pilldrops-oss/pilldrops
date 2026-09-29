package nodeboot

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
)

// The API port speaks TLS and plaintext on one socket, sniffed from the first
// byte. Plaintext stays because the port also serves browser previews that
// would refuse a self-signed certificate; the control plane refuses plaintext hops.

// tlsRecordHandshake is the first byte of a TLS ClientHello. No HTTP method
// starts with it.
const tlsRecordHandshake = 0x16

// sniffTimeout bounds a silent client holding a goroutine and fd. Separate from
// ReadHeaderTimeout, which only starts once http.Server owns the connection.
const sniffTimeout = 10 * time.Second

// Serve runs srv accepting TLS and plaintext on one port. A missing TLS
// identity is not fatal (plaintext only), so a single-box self-hoster is not
// taken offline over a file permission.
func Serve(srv *http.Server, dataDir string) error {
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", srv.Addr)
	if err != nil {
		return err
	}
	id, err := NodeTLS(dataDir)
	if err != nil {
		slog.Error("node TLS identity unavailable; serving plaintext only. A control plane with "+
			"PILLDROPS_REQUIRE_SECURE_HOP=1 will refuse to schedule this node",
			"component", "node", "err", err)
		return srv.Serve(ln)
	}
	slog.Info("node API serving TLS and plaintext on one port",
		"component", "node", "addr", srv.Addr, "tls_spki", id.SPKI)
	return srv.Serve(newProtocolMux(ln, id.ServerConfig()))
}

// Memoised so the heartbeat and the listener cannot race LoadOrCreate on a
// fresh node and end up advertising a pin the listener does not serve.
var (
	nodeTLSOnce sync.Once
	nodeTLSID   *nodetls.Identity
	nodeTLSErr  error
)

// NodeTLS returns this node's TLS identity, creating it on first call.
func NodeTLS(dataDir string) (*nodetls.Identity, error) {
	nodeTLSOnce.Do(func() { nodeTLSID, nodeTLSErr = nodetls.LoadOrCreate(dataDir) })
	return nodeTLSID, nodeTLSErr
}

// NodeTLSSPKI returns the pin to advertise, or "" when there is no identity.
func NodeTLSSPKI(dataDir string) string {
	id, err := NodeTLS(dataDir)
	if err != nil {
		return ""
	}
	return id.SPKI
}

// protocolMux is a net.Listener that classifies each connection, wrapping TLS
// ones in tls.Server so http.Server populates r.TLS. Classification runs in a
// per-connection goroutine so a silent client cannot stall the accept loop.
type protocolMux struct {
	net.Listener
	cfg   *tls.Config
	conns chan net.Conn
	errs  chan error
	// done is closed by Close so in-flight sniff goroutines do not leak.
	done      chan struct{}
	closeOnce sync.Once
}

func newProtocolMux(ln net.Listener, cfg *tls.Config) net.Listener {
	m := &protocolMux{
		Listener: ln,
		cfg:      cfg,
		conns:    make(chan net.Conn),
		errs:     make(chan error, 1),
		done:     make(chan struct{}),
	}
	go m.accept()
	return m
}

func (m *protocolMux) accept() {
	for {
		c, err := m.Listener.Accept()
		if err != nil {
			select {
			case m.errs <- err:
			case <-m.done:
			}
			return
		}
		go m.classify(c)
	}
}

// classify reads the one byte that decides the protocol and forwards the
// connection with that byte pushed back.
func (m *protocolMux) classify(c net.Conn) {
	if err := c.SetReadDeadline(time.Now().Add(sniffTimeout)); err != nil {
		_ = c.Close()
		return
	}
	var first [1]byte
	n, err := io.ReadFull(c, first[:])
	if err != nil || n != 1 {
		_ = c.Close()
		return
	}
	// Clear the sniff deadline or it would abort any request slower than sniffTimeout.
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		_ = c.Close()
		return
	}
	out := net.Conn(&prefixedConn{Conn: c, r: io.MultiReader(bytes.NewReader(first[:]), c)})
	if first[0] == tlsRecordHandshake {
		out = tls.Server(out, m.cfg)
	}
	select {
	case m.conns <- out:
	case <-m.done:
		_ = out.Close()
	}
}

func (m *protocolMux) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case err := <-m.errs:
		return nil, err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *protocolMux) Close() error {
	var err error
	m.closeOnce.Do(func() {
		close(m.done)
		err = m.Listener.Close()
	})
	if err != nil && errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// prefixedConn is a net.Conn whose reads begin with bytes already consumed from
// the underlying connection.
type prefixedConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
