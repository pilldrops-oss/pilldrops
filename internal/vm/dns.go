package vm

// The private resolver a tagged sandbox's own namespace answers on. It knows
// two kinds of name: <slug>.pd, which it answers from the sandbox's peers, and
// everything else, which it hands to the resolvers the template bakes in.
//
// No guest-side change: the namespace redirects the guest's own udp/53 to this
// socket (see taggedDNSRedirect), so a guest keeps its baked resolv.conf and
// an app that hard-codes 8.8.8.8 is answered the same way.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

const (
	// pdZone is the private zone. Trailing dot: dnsmessage names are absolute.
	pdZone = ".pd."
	// pdTTL is short because membership changes take effect immediately; a
	// resolver holding a peer's address for an hour would undo that.
	pdTTL = 30
	// dnsForwardTimeout bounds one upstream query. A guest waiting on a dead
	// resolver should get its own timeout, not this goroutine's.
	dnsForwardTimeout = 4 * time.Second
	// dnsMaxMsg is the UDP payload cap this responder reads and writes. Larger
	// answers are the upstream's business, which sets TC and the client retries.
	dnsMaxMsg = 1500
)

// serveDNS answers queries on pc until pc is closed. peers is asked per query,
// not captured, so a membership change is visible to the next lookup. forward
// is the upstream list; an empty list makes every non-.pd name SERVFAIL.
func serveDNS(pc net.PacketConn, peers, private func() map[string]string, split splitDNS, forward []string) {
	buf := make([]byte, dnsMaxMsg)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			// A closed socket is how this loop is stopped.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		req := make([]byte, n)
		copy(req, buf[:n])
		// One goroutine per query: a forwarded lookup must not stall the
		// answers this resolver can give from memory.
		go func() {
			if resp := answerDNS(req, peers, private, split, forward); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}()
	}
}

// answerDNS builds the reply to one query, or nil when there is nothing to say.
func answerDNS(req []byte, peers, private func() map[string]string, split splitDNS, forward []string) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(req)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	name := q.Name.String()
	if private != nil {
		lower := strings.ToLower(name)
		if ip, ok := private()[lower]; ok {
			if q.Type != dnsmessage.TypeA {
				return buildNoData(hdr.ID, q)
			}
			return buildPDAnswer(hdr.ID, q, ip)
		}
		// Like .pd, the .private zone is ours: an unknown name never leaves the node.
		if strings.HasSuffix(lower, ".private.") {
			return buildPDAnswer(hdr.ID, q, "")
		}
	}
	// After the target names, so a granted target's hostname keeps its
	// 192.168.31.0/24 address even under a granted suffix.
	if split != nil {
		if resp, ok := split(hdr.ID, q); ok {
			return resp
		}
	}
	if !strings.HasSuffix(strings.ToLower(name), pdZone) {
		return forwardDNS(req, forward)
	}
	// Inside the private zone this resolver is authoritative, so an unknown
	// name is NXDOMAIN and never leaves the node.
	slug := strings.ToLower(strings.TrimSuffix(name, pdZone))
	ip := ""
	if q.Type == dnsmessage.TypeA {
		ip = peers()[slug]
	}
	return buildPDAnswer(hdr.ID, q, ip)
}

// buildPDAnswer renders the reply for a .pd name: one A record, or NXDOMAIN
// when ip is "". Authoritative either way — this node is the zone.
func buildPDAnswer(id uint16, q dnsmessage.Question, ip string) []byte {
	h := dnsmessage.Header{ID: id, Response: true, Authoritative: true, RecursionAvailable: true}
	if ip == "" {
		h.RCode = dnsmessage.RCodeNameError
	}
	b := dnsmessage.NewBuilder(nil, h)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil
	}
	if err := b.Question(q); err != nil {
		return nil
	}
	if ip != "" {
		v4 := net.ParseIP(ip).To4()
		if v4 == nil {
			return nil
		}
		if err := b.StartAnswers(); err != nil {
			return nil
		}
		var a dnsmessage.AResource
		copy(a.A[:], v4)
		if err := b.AResource(dnsmessage.ResourceHeader{
			Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: pdTTL,
		}, a); err != nil {
			return nil
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

// buildNoData renders an empty NOERROR reply: the name exists, this type does not.
func buildNoData(id uint16, q dnsmessage.Question) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, Authoritative: true, RecursionAvailable: true})
	if err := b.StartQuestions(); err != nil {
		return nil
	}
	if err := b.Question(q); err != nil {
		return nil
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

// forwardDNS relays a query to the first upstream that answers.
//
// ponytail: the upstream socket is opened wherever this goroutine runs, which
// is the host namespace, not the sandbox's. It makes no difference to the
// answer (the host masquerades the sandbox's own queries to the same
// resolvers), and it is what lets a sandbox under a deny-all egress policy
// still resolve public names, which is the documented behaviour. Open it in
// the namespace if a policy should ever be able to deny name resolution too.
func forwardDNS(req []byte, forward []string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), dnsForwardTimeout)
	defer cancel()
	var d net.Dialer
	for _, up := range forward {
		c, err := d.DialContext(ctx, "udp", net.JoinHostPort(up, "53"))
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(dnsForwardTimeout))
		if _, err := c.Write(req); err != nil {
			_ = c.Close()
			continue
		}
		buf := make([]byte, dnsMaxMsg)
		n, err := c.Read(buf)
		_ = c.Close()
		if err != nil {
			continue
		}
		return buf[:n]
	}
	return nil
}

// splitDNS answers a query under a granted suffix; ok is false for any other
// name, which then resolves as it did before.
type splitDNS func(id uint16, q dnsmessage.Question) (resp []byte, ok bool)

// maxSplitAnswers is the most records the hub sends; more is a misbehaving hub.
const maxSplitAnswers = 32

// splitAnswer builds the reply to a split-DNS query. AAAA is empty, types other
// than A and CNAME are NOTIMP, and a failed relay (any non-200, which relay
// reports as an error) or an rcode outside 0..15 is SERVFAIL, never a public
// lookup. relay returns the chain in order; each A record goes through learn,
// which routes the address before this returns and gives the TTL the guest may
// cache. learn's false means the address could not be routed, so the reply is
// SERVFAIL: the guest would otherwise connect around the proxy.
func splitAnswer(id uint16, q dnsmessage.Question, relay func(qtype string) (sandbox.PrivateDNSReply, error), learn func(a netip.Addr, ttl uint32) (uint32, bool)) []byte {
	var qtype string
	switch q.Type {
	case dnsmessage.TypeAAAA:
		return buildNoData(id, q)
	case dnsmessage.TypeA:
		qtype = "A"
	case dnsmessage.TypeCNAME:
		qtype = "CNAME"
	default:
		return buildRCode(id, q, dnsmessage.RCodeNotImplemented)
	}
	reply, err := relay(qtype)
	if err != nil || reply.RCode < 0 || reply.RCode > 15 {
		return buildRCode(id, q, dnsmessage.RCodeServerFailure)
	}
	if reply.RCode != 0 {
		return buildRCode(id, q, dnsmessage.RCode(reply.RCode))
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, RecursionAvailable: true})
	b.EnableCompression()
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return buildRCode(id, q, dnsmessage.RCodeServerFailure)
	}
	owner := q.Name
	for _, a := range reply.Answers[:min(len(reply.Answers), maxSplitAnswers)] {
		h := dnsmessage.ResourceHeader{Name: owner, Class: dnsmessage.ClassINET, TTL: min(a.TTL, uint32(dnsMaxTimeout/time.Second))}
		switch a.Type {
		case "CNAME":
			next, err := dnsmessage.NewName(strings.TrimSuffix(a.Value, ".") + ".")
			if err != nil {
				return buildRCode(id, q, dnsmessage.RCodeServerFailure)
			}
			h.Type = dnsmessage.TypeCNAME
			if b.CNAMEResource(h, dnsmessage.CNAMEResource{CNAME: next}) != nil {
				return buildRCode(id, q, dnsmessage.RCodeServerFailure)
			}
			owner = next
		case "A":
			ip, err := netip.ParseAddr(a.Value)
			// Platform addresses are never routed, as the hub also drops them.
			if err != nil || !ip.Is4() || len(sandbox.Shadowed(netip.PrefixFrom(ip, 32))) > 0 {
				continue
			}
			ttl, ok := learn(ip, a.TTL)
			if !ok {
				return buildRCode(id, q, dnsmessage.RCodeServerFailure)
			}
			h.Type, h.TTL = dnsmessage.TypeA, ttl
			if b.AResource(h, dnsmessage.AResource{A: ip.As4()}) != nil {
				return buildRCode(id, q, dnsmessage.RCodeServerFailure)
			}
		}
	}
	out, err := b.Finish()
	if err != nil {
		return buildRCode(id, q, dnsmessage.RCodeServerFailure)
	}
	return out
}

// buildRCode renders an empty reply carrying rcode.
func buildRCode(id uint16, q dnsmessage.Question, rcode dnsmessage.RCode) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, RecursionAvailable: true, RCode: rcode})
	if b.StartQuestions() != nil || b.Question(q) != nil {
		return nil
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}
