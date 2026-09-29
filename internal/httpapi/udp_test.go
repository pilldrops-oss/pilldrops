package httpapi

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// udpEchoListener answers every datagram with the same bytes, to the sender.
func udpEchoListener(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	// Big buffers so the burst below is not lost to kernel drops; a dropped
	// datagram would look like the relay hanging.
	_ = pc.SetReadBuffer(8 << 20)  //nolint:errcheck // best-effort tuning
	_ = pc.SetWriteBuffer(8 << 20) //nolint:errcheck // best-effort tuning
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(buf[:n], from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// openUDPTunnel performs the ingress's upgrade against the /udp/ route.
func openUDPTunnel(t *testing.T, addr, id string, port int, tok string) (net.Conn, int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/v1/sandboxes/" + id + "/udp/" + strconv.Itoa(port), RawQuery: "ptok=" + url.QueryEscape(tok)},
		Proto:  "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Host:   addr,
		Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"pilldrops-l4-udp"}},
	}
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = c.Close()
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { _ = c.Close() })
	// A small receive window on the client is what makes the relay's writes
	// block while the client is still sending. Without it 1.6 MB fits in
	// loopback buffers and a relay that deadlocks on a real hop still passes.
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096) //nolint:errcheck // best-effort tuning
	}
	return prefixed{Conn: c, r: br}, resp.StatusCode
}

// 200 datagrams of random length must come back with matching bodies in order,
// and two client addresses interleaved must not receive each other's replies.
func TestUDPTunnelPreservesBoundariesAndClientIdentity(t *testing.T) {
	addr, id, org, _ := forwardNode(t)
	port := udpEchoListener(t)
	tok := previewtoken.Token([]byte(testPreviewSecret), id, port, org)

	conn, code := openUDPTunnel(t, addr, id, port, tok)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("udp tunnel handshake = %d, want 101", code)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	clientA := netip.MustParseAddrPort("198.51.100.7:40001")
	clientB := netip.MustParseAddrPort("198.51.100.8:40002")

	const n = 200
	sent := map[netip.AddrPort][][]byte{clientA: {}, clientB: {}}
	// Sent before anything is read, like any client that does not wait for a
	// reply between datagrams. That is what makes the stream write block while
	// the relay is still being fed, which is the deadlock this catches.
	for i := 0; i < n; i++ {
		who := clientA
		if i%2 == 1 {
			who = clientB
		}
		// ~1.6 MB each way. This does NOT exercise stream back-pressure: on
		// loopback that much fits in socket buffers, and a relay that holds its
		// peer map across the stream write passes here. Raising it far enough
		// to provoke that stalls the correct code too (the in-process echo
		// cannot drain 10 MB of 60 KB datagrams), so it is left unguarded and
		// said so rather than guarded by a flaky test.
		size := 1 + i%16000
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		// Tag the payload with its owner so a misrouted reply is provable, not
		// just "a length that happened to match".
		copy(payload, fmt.Sprintf("%s#%03d", who.Addr(), i))
		sent[who] = append(sent[who], payload)
		if err := httpx.WriteDatagram(conn, who, payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	got := map[netip.AddrPort][][]byte{clientA: {}, clientB: {}}
	for i := 0; i < n; i++ {
		who, payload, err := httpx.ReadDatagram(conn)
		if err != nil {
			t.Fatalf("read %d of %d: %v", i, n, err)
		}
		if _, known := got[who]; !known {
			t.Fatalf("reply %d came back for %v, which never sent anything", i, who)
		}
		got[who] = append(got[who], payload)
	}

	for who, want := range sent {
		have := got[who]
		if len(have) != len(want) {
			t.Fatalf("%v got %d replies, sent %d", who, len(have), len(want))
		}
		for i := range want {
			if !bytes.Equal(have[i], want[i]) {
				t.Fatalf("%v reply %d differs: got %d bytes %q, sent %d bytes %q",
					who, i, len(have[i]), head(have[i]), len(want[i]), head(want[i]))
			}
		}
	}
}

func head(b []byte) []byte {
	if len(b) > 12 {
		return b[:12]
	}
	return b
}

// The udp route is gated exactly like the tcp one.
func TestUDPTunnelRefusesWithoutTheToken(t *testing.T) {
	addr, id, org, _ := forwardNode(t)
	port := udpEchoListener(t)
	if _, code := openUDPTunnel(t, addr, id, port, ""); code != http.StatusNotFound {
		t.Errorf("no token = %d, want 404", code)
	}
	good := previewtoken.Token([]byte(testPreviewSecret), id, port, org)
	if _, code := openUDPTunnel(t, addr, id, port, good); code != http.StatusSwitchingProtocols {
		t.Fatalf("valid token = %d, want 101 — the refusal above proves nothing if this fails too", code)
	}
}

// A payload larger than one frame is refused rather than split: splitting would
// deliver two datagrams where the caller sent one.
func TestDatagramFramingRoundTripsAndRefusesOversize(t *testing.T) {
	var buf bytes.Buffer
	who := netip.MustParseAddrPort("[2001:db8::1]:5353")
	for _, size := range []int{0, 1, 1500, httpx.MaxDatagram} {
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		if err := httpx.WriteDatagram(&buf, who, payload); err != nil {
			t.Fatalf("write %d bytes: %v", size, err)
		}
		gotWho, gotPayload, err := httpx.ReadDatagram(&buf)
		if err != nil {
			t.Fatalf("read %d bytes: %v", size, err)
		}
		if gotWho != who || !bytes.Equal(gotPayload, payload) {
			t.Fatalf("%d-byte frame came back as %v / %d bytes", size, gotWho, len(gotPayload))
		}
	}
	if err := httpx.WriteDatagram(io.Discard, who, make([]byte, httpx.MaxDatagram+1)); err == nil {
		t.Fatal("an oversize payload was framed instead of refused")
	}
}
