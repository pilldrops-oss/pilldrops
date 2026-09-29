package httpx

import (
	"io"
	"net"
	"sync"
)

// halfCloser is implemented by *net.TCPConn and *tls.Conn: the write direction
// can be shut without tearing down the read direction.
type halfCloser interface{ CloseWrite() error }

// Splice copies bytes both ways between two connections until both directions
// have ended, then closes both. When one direction reaches EOF the other side's
// write half is closed rather than the whole connection, so a protocol that
// signals end-of-request with a half-close (an HTTP client, `nc -N`, a database
// wire protocol) still gets its answer back.
func Splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src) //nolint:errcheck // a broken pipe here is the peer hanging up, which is normal
		if hc, ok := dst.(halfCloser); ok {
			_ = hc.CloseWrite() //nolint:errcheck // best-effort: the full Close below is the backstop
			return
		}
		_ = dst.Close() //nolint:errcheck // best-effort
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	_ = a.Close() //nolint:errcheck // best-effort
	_ = b.Close() //nolint:errcheck // best-effort
}

// Prefixed returns a conn whose reads start with the bytes already buffered in
// r and continue on c. Hand-rolled HTTP upgrades leave bytes in the bufio
// reader that read the response; losing them silently truncates the stream.
func Prefixed(c net.Conn, r io.Reader) net.Conn { return &prefixedConn{Conn: c, r: r} }

type prefixedConn struct {
	net.Conn
	r io.Reader
}

func (p *prefixedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// CloseWrite keeps the half-close available through the wrapper.
func (p *prefixedConn) CloseWrite() error {
	if hc, ok := p.Conn.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return p.Close()
}
