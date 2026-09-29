package vm

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// query builds a wire-format A query, so the parser under test is fed the same
// bytes a guest resolver would send.
func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatalf("name %q: %v", name, err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// parse returns the rcode and the first A answer of a response.
func parse(t *testing.T, msg []byte) (dnsmessage.RCode, string) {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(msg); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if !m.Response {
		t.Fatal("reply is not marked as a response")
	}
	if m.ID != 0x1234 {
		t.Fatalf("reply id %#x, want the query's — a mismatched id is discarded by every resolver", m.ID)
	}
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			if a.Header.TTL != pdTTL {
				t.Errorf("TTL %d, want %d: membership changes take effect at once and a long TTL would undo that", a.Header.TTL, pdTTL)
			}
			return m.RCode, net.IP(r.A[:]).String()
		}
	}
	return m.RCode, ""
}

func TestPrivateZoneAnswers(t *testing.T) {
	t.Parallel()
	peers := func() map[string]string { return map[string]string{"bravo": "10.128.6.70"} }
	// No upstream: a forwarded name must be visibly unanswered rather than
	// quietly answered from the zone.
	none := []string(nil)

	rcode, ip := parse(t, answerDNS(query(t, "bravo.pd.", dnsmessage.TypeA), peers, none))
	if rcode != dnsmessage.RCodeSuccess || ip != "10.128.6.70" {
		t.Fatalf("bravo.pd -> %v/%q, want the peer's address", rcode, ip)
	}

	// Case folding: a guest that upper-cases the name gets the same answer.
	if rcode, ip := parse(t, answerDNS(query(t, "BRAVO.pd.", dnsmessage.TypeA), peers, none)); rcode != dnsmessage.RCodeSuccess || ip != "10.128.6.70" {
		t.Errorf("BRAVO.pd -> %v/%q, want the same answer as the lowercase form", rcode, ip)
	}

	// A name in the zone that is not a peer is NXDOMAIN, and never forwarded:
	// this resolver is authoritative for .pd.
	if rcode, _ := parse(t, answerDNS(query(t, "charlie.pd.", dnsmessage.TypeA), peers, none)); rcode != dnsmessage.RCodeNameError {
		t.Errorf("charlie.pd -> %v, want NXDOMAIN", rcode)
	}
	// An untagged sandbox has no peers at all, so every .pd name is NXDOMAIN.
	empty := func() map[string]string { return nil }
	if rcode, _ := parse(t, answerDNS(query(t, "bravo.pd.", dnsmessage.TypeA), empty, none)); rcode != dnsmessage.RCodeNameError {
		t.Errorf("bravo.pd with no peers -> %v, want NXDOMAIN", rcode)
	}
	// AAAA for a peer: the zone has no IPv6 records, so NXDOMAIN rather than a
	// forward that would leak the name and answer from the public internet.
	if rcode, _ := parse(t, answerDNS(query(t, "bravo.pd.", dnsmessage.TypeAAAA), peers, none)); rcode != dnsmessage.RCodeNameError {
		t.Errorf("bravo.pd AAAA -> %v, want NXDOMAIN", rcode)
	}
	// Anything outside the zone is forwarded; with no upstream there is nothing
	// to say, which is the control that .pd answers did not come from a
	// catch-all.
	if got := answerDNS(query(t, "example.com.", dnsmessage.TypeA), peers, none); got != nil {
		t.Errorf("example.com produced a local answer (%d bytes); it must be forwarded", len(got))
	}
}

// End to end over a real socket, so the serve loop and the wire format are
// exercised, not just the builder. The forwarding half is not unit-tested: it
// dials port 53 by construction and is measured live instead (a tagged sandbox
// still resolving example.com).
func TestServeDNSOverASocket(t *testing.T) {
	t.Parallel()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	peers := func() map[string]string { return map[string]string{"alpha": "10.128.6.66"} }
	go serveDNS(pc, peers, nil)

	c, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(query(t, "alpha.pd.", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, dnsMaxMsg)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if rcode, ip := parse(t, buf[:n]); rcode != dnsmessage.RCodeSuccess || ip != "10.128.6.66" {
		t.Fatalf("over the socket: alpha.pd -> %v/%q", rcode, ip)
	}
}
