package vm

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
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

	rcode, ip := parse(t, answerDNS(query(t, "bravo.pd.", dnsmessage.TypeA), peers, nil, nil, none))
	if rcode != dnsmessage.RCodeSuccess || ip != "10.128.6.70" {
		t.Fatalf("bravo.pd -> %v/%q, want the peer's address", rcode, ip)
	}

	// Case folding: a guest that upper-cases the name gets the same answer.
	if rcode, ip := parse(t, answerDNS(query(t, "BRAVO.pd.", dnsmessage.TypeA), peers, nil, nil, none)); rcode != dnsmessage.RCodeSuccess || ip != "10.128.6.70" {
		t.Errorf("BRAVO.pd -> %v/%q, want the same answer as the lowercase form", rcode, ip)
	}

	// A name in the zone that is not a peer is NXDOMAIN, and never forwarded:
	// this resolver is authoritative for .pd.
	if rcode, _ := parse(t, answerDNS(query(t, "charlie.pd.", dnsmessage.TypeA), peers, nil, nil, none)); rcode != dnsmessage.RCodeNameError {
		t.Errorf("charlie.pd -> %v, want NXDOMAIN", rcode)
	}
	// An untagged sandbox has no peers at all, so every .pd name is NXDOMAIN.
	empty := func() map[string]string { return nil }
	if rcode, _ := parse(t, answerDNS(query(t, "bravo.pd.", dnsmessage.TypeA), empty, nil, nil, none)); rcode != dnsmessage.RCodeNameError {
		t.Errorf("bravo.pd with no peers -> %v, want NXDOMAIN", rcode)
	}
	// AAAA for a peer: the zone has no IPv6 records, so NXDOMAIN rather than a
	// forward that would leak the name and answer from the public internet.
	if rcode, _ := parse(t, answerDNS(query(t, "bravo.pd.", dnsmessage.TypeAAAA), peers, nil, nil, none)); rcode != dnsmessage.RCodeNameError {
		t.Errorf("bravo.pd AAAA -> %v, want NXDOMAIN", rcode)
	}
	// Anything outside the zone is forwarded; with no upstream there is nothing
	// to say, which is the control that .pd answers did not come from a
	// catch-all.
	if got := answerDNS(query(t, "example.com.", dnsmessage.TypeA), peers, nil, nil, none); got != nil {
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
	go serveDNS(pc, peers, nil, nil, nil)

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

func TestPrivateZoneAnswersAndRefusesUnknown(t *testing.T) {
	t.Parallel()
	peers := func() map[string]string { return nil }
	priv := func() map[string]string {
		return map[string]string{"db.private.": "192.168.31.2", "internal.corp.": "192.168.31.3"}
	}
	for name, want := range map[string]string{"db.private.": "192.168.31.2", "INTERNAL.corp.": "192.168.31.3"} {
		if rcode, ip := parse(t, answerDNS(query(t, name, dnsmessage.TypeA), peers, priv, nil, nil)); rcode != dnsmessage.RCodeSuccess || ip != want {
			t.Errorf("%s -> %v/%q, want %q", name, rcode, ip, want)
		}
	}
	// forward points at TEST-NET, so a leaked query would time out and fail here.
	if rcode, _ := parse(t, answerDNS(query(t, "nope.private.", dnsmessage.TypeA), peers, priv, nil, []string{"192.0.2.1"})); rcode != dnsmessage.RCodeNameError {
		t.Errorf("unknown .private: %v", rcode)
	}
	if rcode, ip := parse(t, answerDNS(query(t, "db.private.", dnsmessage.TypeAAAA), peers, priv, nil, nil)); rcode != dnsmessage.RCodeSuccess || ip != "" {
		t.Errorf("AAAA: %v/%q", rcode, ip)
	}
}

func unpack(t *testing.T, msg []byte) dnsmessage.Message {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(msg); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	return m
}

func TestSplitAnswer(t *testing.T) {
	t.Parallel()
	name, _ := dnsmessage.NewName("wiki.corp.internal.")
	q := func(typ dnsmessage.Type) dnsmessage.Question {
		return dnsmessage.Question{Name: name, Type: typ, Class: dnsmessage.ClassINET}
	}
	chain := func(string) (sandbox.PrivateDNSReply, error) {
		return sandbox.PrivateDNSReply{Answers: []sandbox.PrivateDNSAnswer{
			{Type: "CNAME", Value: "web.corp.internal", TTL: 600},
			{Type: "A", Value: "10.9.9.9", TTL: 600},
		}}, nil
	}
	var learned []string
	learn := func(a netip.Addr, ttl uint32) (uint32, bool) {
		learned = append(learned, a.String())
		return 42, true
	}
	m := unpack(t, splitAnswer(7, q(dnsmessage.TypeA), chain, learn))
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 2 || len(learned) != 1 || learned[0] != "10.9.9.9" {
		t.Fatalf("chain: %+v learned %v", m, learned)
	}
	cn, ok := m.Answers[0].Body.(*dnsmessage.CNAMEResource)
	if !ok || m.Answers[0].Header.Name.String() != "wiki.corp.internal." || cn.CNAME.String() != "web.corp.internal." || m.Answers[0].Header.TTL != 300 {
		t.Fatalf("cname record %+v", m.Answers[0])
	}
	if m.Answers[1].Header.Name.String() != "web.corp.internal." || m.Answers[1].Header.TTL != 42 {
		t.Fatalf("A record must hang off the CNAME target with learn's TTL: %+v", m.Answers[1].Header)
	}
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeAAAA), chain, learn)); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Errorf("AAAA: %+v", m)
	}
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeMX), chain, learn)); m.RCode != dnsmessage.RCodeNotImplemented {
		t.Errorf("MX: %v", m.RCode)
	}
	down := func(string) (sandbox.PrivateDNSReply, error) { return sandbox.PrivateDNSReply{}, errors.New("hub unreachable") }
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeA), down, learn)); m.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("unreachable hub must SERVFAIL, never fall back to a public lookup: %v", m.RCode)
	}
	nx := func(string) (sandbox.PrivateDNSReply, error) { return sandbox.PrivateDNSReply{RCode: 3}, nil }
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeA), nx, learn)); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("NXDOMAIN: %v", m.RCode)
	}
	wild := func(string) (sandbox.PrivateDNSReply, error) { return sandbox.PrivateDNSReply{RCode: 4095}, nil }
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeA), wild, learn)); m.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("an rcode outside 0..15 must become SERVFAIL: %v", m.RCode)
	}
	many := func(string) (sandbox.PrivateDNSReply, error) {
		var r sandbox.PrivateDNSReply
		for i := 0; i < 40; i++ {
			r.Answers = append(r.Answers, sandbox.PrivateDNSAnswer{Type: "A", Value: "10.9.9.9", TTL: 60})
		}
		return r, nil
	}
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeA), many, learn)); len(m.Answers) != 32 {
		t.Errorf("answers must be capped at 32: %d", len(m.Answers))
	}
	platform := func(string) (sandbox.PrivateDNSReply, error) {
		return sandbox.PrivateDNSReply{Answers: []sandbox.PrivateDNSAnswer{{Type: "A", Value: "10.128.0.6", TTL: 60}, {Type: "A", Value: "192.168.31.2", TTL: 60}}}, nil
	}
	learned = nil
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeA), platform, learn)); len(m.Answers) != 0 || len(learned) != 0 {
		t.Errorf("platform addresses must be neither answered nor learned: %+v learned %v", m.Answers, learned)
	}
	refused := func(netip.Addr, uint32) (uint32, bool) { return 0, false }
	if m := unpack(t, splitAnswer(7, q(dnsmessage.TypeA), chain, refused)); m.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("an address the set could not take must not be handed to the guest: %v", m.RCode)
	}
}

func TestSplitAnswerQueryTypesAndAddresses(t *testing.T) {
	t.Parallel()
	name, _ := dnsmessage.NewName("wiki.corp.internal.")
	var asked string
	relay := func(qtype string) (sandbox.PrivateDNSReply, error) {
		asked = qtype
		return sandbox.PrivateDNSReply{Answers: []sandbox.PrivateDNSAnswer{
			{Type: "CNAME", Value: "web.corp.internal", TTL: 60},
			{Type: "A", Value: "2001:db8::1", TTL: 60},
			{Type: "A", Value: "not-an-address", TTL: 60},
			{Type: "A", Value: "10.9.9.9", TTL: 600},
		}}, nil
	}
	var learnedTTLs []uint32
	learn := func(_ netip.Addr, ttl uint32) (uint32, bool) {
		learnedTTLs = append(learnedTTLs, ttl)
		_, reply := dnsTimeouts(ttl)
		return reply, true
	}
	m := unpack(t, splitAnswer(7, dnsmessage.Question{Name: name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, relay, learn))
	if asked != "CNAME" {
		t.Errorf("relayed qtype %q, want CNAME", asked)
	}
	if len(m.Answers) != 2 || len(learnedTTLs) != 1 {
		t.Fatalf("only the CNAME and the IPv4 A may be answered: %+v learned %v", m.Answers, learnedTTLs)
	}
	if learnedTTLs[0] != 600 || m.Answers[1].Header.TTL != 300 {
		t.Errorf("learn got TTL %d and the guest %d, want 600 and the clamped 300", learnedTTLs[0], m.Answers[1].Header.TTL)
	}
}

func TestTargetHostsAnswerBeforeSplitDNS(t *testing.T) {
	t.Parallel()
	priv := func() map[string]string { return map[string]string{"wiki.corp.internal.": "192.168.31.2"} }
	split := func(id uint16, q dnsmessage.Question) ([]byte, bool) {
		return buildRCode(id, q, dnsmessage.RCodeRefused), true
	}
	if rcode, ip := parse(t, answerDNS(query(t, "wiki.corp.internal.", dnsmessage.TypeA), nil, priv, split, nil)); rcode != dnsmessage.RCodeSuccess || ip != "192.168.31.2" {
		t.Fatalf("target host: %v %q", rcode, ip)
	}
	m := unpack(t, answerDNS(query(t, "mail.corp.internal.", dnsmessage.TypeA), nil, priv, split, nil))
	if m.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("split name: %v", m.RCode)
	}
}
