package egressproxy

import (
	"bytes"
	"crypto/tls"
	"net"
	"net/netip"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// writeRecorder keeps what the client put on the wire, so the test can assert
// the proxy replays it byte for byte rather than approximately.
type writeRecorder struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *writeRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.mu.Unlock()
	return w.Conn.Write(p)
}

func (w *writeRecorder) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// A real TLS client, a real ClientHello, and no certificate anywhere on the
// platform side. The two things that must both hold: the name is read, and the
// handshake never completes — a proxy that terminated TLS would pass the first
// half of this test and fail the second.
func TestSNIIsReadAndTheSessionIsNotTerminated(t *testing.T) {
	c, s := net.Pipe()
	rec := &writeRecorder{Conn: c}

	done := make(chan *tls.Conn, 1)
	go func() {
		tc := tls.Client(rec, &tls.Config{ServerName: "api.github.com"})
		_ = tc.Handshake() // always fails: nothing on the other side answers
		done <- tc
	}()

	name, replay := sniffName(s, egress.PortHTTPS)
	if name != "api.github.com" {
		t.Fatalf("SNI read as %q, want api.github.com — a domain rule decides nothing without it", name)
	}
	_ = s.Close()
	tc := <-done

	if tc.ConnectionState().HandshakeComplete {
		t.Error("the TLS handshake COMPLETED against the name layer: the platform terminated a " +
			"tenant's TLS session and its plaintext is in this process")
	}
	if got := rec.bytes(); !bytes.Equal(got, replay) {
		t.Errorf("the replay is not what the client sent (%d bytes read, %d written); the upstream "+
			"would receive a mangled or truncated ClientHello", len(replay), len(got))
	}
	if len(replay) == 0 || replay[0] != 0x16 {
		t.Errorf("the replay does not start with a TLS handshake record: %x", replay[:min(8, len(replay))])
	}
}

// Not TLS at all on 443. No name, every byte still recorded, and the address
// decides — which is the documented behaviour, not an error path.
func TestGarbageOn443HasNoNameAndIsStillReplayable(t *testing.T) {
	c, s := net.Pipe()
	go func() {
		_, _ = c.Write([]byte("PING\r\n"))
	}()
	name, replay := sniffName(s, egress.PortHTTPS)
	if name != "" {
		t.Errorf("a name %q was invented for a connection that carries none", name)
	}
	if !bytes.Contains(replay, []byte("PING")) {
		t.Errorf("the bytes read off a non-TLS connection were dropped instead of replayed: %q", replay)
	}
}

func TestHostHeaderIsRead(t *testing.T) {
	c, s := net.Pipe()
	req := "GET /x HTTP/1.1\r\nHost: files.example.org:80\r\nUser-Agent: t\r\n\r\n"
	go func() {
		_, _ = c.Write([]byte(req))
	}()
	name, replay := sniffName(s, egress.PortHTTP)
	if name != "files.example.org" {
		t.Fatalf("Host read as %q, want files.example.org (port stripped)", name)
	}
	if string(replay) != req {
		t.Errorf("the request line and headers were not replayed intact:\n%q", replay)
	}
}

// Nothing is read on a port where no name can be on the wire. A domain rule
// must not be able to decide a Postgres connection, and reading would only add
// latency to one it can never match.
func TestNoSniffOnAPortWithNoNameOnTheWire(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	name, replay := sniffName(s, 5432)
	if name != "" || replay != nil {
		t.Errorf("port 5432 was sniffed: name %q, %d bytes", name, len(replay))
	}
}

// The sniffer must never put a byte on a tenant's connection.
func TestTheSnifferCannotWriteToTheGuest(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	if _, err := (&recorder{Conn: s}).Write([]byte("hello")); err != ErrNoWrite {
		t.Errorf("the recorder wrote to the tenant connection (err %v)", err)
	}
}

// An SNI-less ClientHello must not be permitted on the strength of a domain
// entry it cannot be matched against. This is the matcher's contract, asserted
// here because it is the name layer that supplies the empty name.
func TestSNILessTLSCannotBorrowADomainAllow(t *testing.T) {
	allow, deny, err := (&egress.Rules{Allow: []string{"*.github.com"}, Deny: []string{egress.AllTraffic}}).Parse()
	if err != nil {
		t.Fatal(err)
	}
	dst := netip.MustParseAddr("140.82.121.4")
	if v := egress.Match(allow, deny, egress.Conn{Addr: dst, Port: 443}); v.Allowed {
		t.Errorf("a nameless connection was allowed by a domain entry: %s", v.Reason)
	}
	// The same connection, once an address entry names it, is judged on that.
	allow2, deny2, _ := (&egress.Rules{
		Allow: []string{"*.github.com", "140.82.121.4"}, Deny: []string{egress.AllTraffic},
	}).Parse()
	if v := egress.Match(allow2, deny2, egress.Conn{Addr: dst, Port: 443}); !v.Allowed {
		t.Errorf("an address entry did not permit a nameless connection to that address: %s", v.Reason)
	}
}

// The dial leaves the node from OUTSIDE the sandbox's namespace, so the in-ns
// drops that keep a guest off the node and the metadata service are not in its
// path. An allow-listed name that resolves to 169.254.169.254 is the attack.
func TestAResolvedPrivateAddressIsRefusedAtDialTime(t *testing.T) {
	for _, addr := range []string{"169.254.169.254:80", "10.128.0.29:443", "127.0.0.1:443", "192.168.30.1:80"} {
		if err := refusePrivate("tcp4", addr, nil); err == nil {
			t.Errorf("%s was accepted as an upstream: an allow-listed name resolving there is a "+
				"route to the node, another sandbox or the metadata service", addr)
		}
	}
	if err := refusePrivate("tcp4", "140.82.121.4:443", nil); err != nil {
		t.Errorf("a public destination was refused: %v", err)
	}
}

// A domain verdict is dialled by NAME, resolved in the same decision; an
// address verdict is dialled at the address the rule named. Getting this
// backwards is what makes DNS rebinding work.
func TestTheDialTargetFollowsTheDecidingEntry(t *testing.T) {
	allow, deny, err := (&egress.Rules{
		Allow: []string{"*.github.com", "203.0.113.7"}, Deny: []string{egress.AllTraffic},
	}).Parse()
	if err != nil {
		t.Fatal(err)
	}
	byName := egress.Match(allow, deny, egress.Conn{Name: "api.github.com", Addr: netip.MustParseAddr("203.0.113.9"), Port: 443})
	if !byName.Allowed || byName.Entry.Kind != egress.KindDomain {
		t.Fatalf("a name-permitted connection did not report a domain entry: %+v", byName)
	}
	byAddr := egress.Match(allow, deny, egress.Conn{Addr: netip.MustParseAddr("203.0.113.7"), Port: 443})
	if !byAddr.Allowed || byAddr.Entry.Kind != egress.KindAddr {
		t.Fatalf("an address-permitted connection did not report an address entry: %+v", byAddr)
	}
}
