package egressproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// sniffDeadline bounds how long a connection may take to say where it is
// going. A guest that opens a socket and says nothing must not hold a
// goroutine; it simply presents no name and is judged on address.
const sniffDeadline = 5 * time.Second

// sniffName reads the hostname a connection announces and returns it with
// every byte consumed, so the caller can replay them upstream untouched.
//
// A name is only on the wire for TLS (SNI) and plaintext HTTP (Host); on any
// other port nothing is read at all, because there is no name to find and
// reading would only delay a connection a domain rule can never decide.
// An empty name is a normal answer, not a failure: it means the connection is
// judged on its address alone, which is what the rules say happens to a
// ClientHello with no SNI or a protocol that is not HTTP.
func sniffName(c net.Conn, port int) (name string, replay []byte) {
	if !egress.NameIsOnTheWire(port) {
		return "", nil
	}
	_ = c.SetReadDeadline(time.Now().Add(sniffDeadline))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()

	r := &recorder{Conn: c}
	if port == egress.PortHTTPS {
		name = sniffSNI(r)
	} else {
		name = sniffHost(r)
	}
	return name, r.buf.Bytes()
}

// errStopHandshake aborts the handshake as soon as the ClientHello is parsed.
// Reaching this error is the SUCCESS path: it is how the TLS session is left
// un-terminated, with the ClientHello in the recorder and no reply sent.
var errStopHandshake = errors.New("clienthello read; not terminating this session")

// sniffSNI parses the ClientHello with crypto/tls rather than by hand. The
// stdlib already knows every shape of that message, and a second parser in
// this repo would be a second place for a malformed hello to be read wrongly.
func sniffSNI(r *recorder) string {
	var name string
	cfg := &tls.Config{
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			name = chi.ServerName
			return nil, errStopHandshake
		},
	}
	// The error is always non-nil and always ignored: either the hello parsed
	// and we stopped it, or it was not a TLS hello at all — in which case there
	// is no name, the bytes are still recorded, and the address decides.
	_ = tls.Server(r, cfg).Handshake()
	return name
}

// sniffHost reads the request line and headers of a plaintext HTTP request and
// returns its Host, port stripped. The body is never read: ReadRequest stops
// at the headers, so an upload is spliced rather than buffered here.
func sniffHost(r *recorder) string {
	req, err := http.ReadRequest(bufio.NewReader(r))
	if err != nil {
		return ""
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSpace(host)
}

// recorder keeps every byte read from the connection so the sniffer's reads
// can be replayed to the upstream. Writes are refused: nothing this package
// does may put a byte on a tenant's connection before the splice, which is
// what makes "no TLS termination" a property of the type and not of a comment.
type recorder struct {
	net.Conn
	buf bytes.Buffer
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.buf.Write(p[:n])
	return n, err
}

// ErrNoWrite is what a sniffer gets if it tries to answer the guest.
var ErrNoWrite = errors.New("the name layer never writes to a tenant connection")

func (r *recorder) Write([]byte) (int, error) { return 0, ErrNoWrite }
