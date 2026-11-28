package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

func (ts *connectorTestServer) nodeDNS(t *testing.T, key, sb, name, qtype string) (int, sandbox.PrivateDNSReply) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"sandbox": sb, "name": name, "qtype": qtype})
	req, _ := http.NewRequest(http.MethodPost, ts.srv.URL+"/internal/private/dns", bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := ts.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out sandbox.PrivateDNSReply
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func wikiAnswers(name, qtype string) map[string]any {
	if qtype != "A" || name != "wiki.corp.internal" {
		return map[string]any{"rcode": 3, "answers": []any{}}
	}
	return map[string]any{"rcode": 0, "answers": []any{
		map[string]any{"type": "CNAME", "value": "web.corp.internal", "ttl": 600},
		map[string]any{"type": "A", "value": "10.9.9.9", "ttl": 600},
		map[string]any{"type": "A", "value": "169.254.169.254", "ttl": 60},
		map[string]any{"type": "CNAME", "value": "bad_host..corp", "ttl": 60},
	}}
}

// dnsGrant gives sb1, on a routes node, a DNS grant for corp.internal from prod.
func (ts *connectorTestServer) dnsGrant(t *testing.T, dns func(name, qtype string) map[string]any) *privateFakeNode {
	t.Helper()
	node := ts.routesNode(t)
	dialInstanceWith(t, ts, "prod", "inst-aaaaaaaaaaaa", routesCaps, true, dns)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","dns":"corp.internal"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("dns grant: %d %s", resp.StatusCode, ts.body(resp))
	}
	return node
}

func TestDNSRelayAnswersAndRemembers(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.dnsGrant(t, wikiAnswers)
	code, reply := ts.nodeDNS(t, node.key, "sb1", "Wiki.Corp.Internal.", "A")
	if code != http.StatusOK || reply.RCode != 0 || len(reply.Answers) != 2 {
		t.Fatalf("%d %+v", code, reply)
	}
	if reply.Answers[0].Type != "CNAME" || reply.Answers[1].Value != "10.9.9.9" || reply.Answers[1].TTL != 300 {
		t.Fatalf("answers %+v: want the CNAME, then the A capped at 300 s, and no link-local answer or bad CNAME", reply.Answers)
	}
	e, ok := ts.s.privateDNS.lookup("sb1", netip.MustParseAddr("10.9.9.9"), time.Now())
	if !ok || e.suffix != "corp.internal" || e.name != "wiki.corp.internal" || e.instanceID != "inst-aaaaaaaaaaaa" {
		t.Fatalf("not remembered: %+v %v", e, ok)
	}
	if _, ok := ts.s.privateDNS.lookup("sb1", netip.MustParseAddr("169.254.169.254"), time.Now()); ok {
		t.Fatal("a platform address was remembered")
	}
	if code, reply := ts.nodeDNS(t, node.key, "sb1", "nope.corp.internal", "A"); code != http.StatusOK || reply.RCode != 3 {
		t.Fatalf("NXDOMAIN passes through: %d %+v", code, reply)
	}
}

func TestDNSRelayRefusals(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.dnsGrant(t, func(name, qtype string) map[string]any {
		switch name {
		case "slow.corp.internal":
			return nil
		case "rcode.corp.internal":
			return map[string]any{"rcode": 16, "answers": []any{}}
		case "big.corp.internal":
			many := make([]any, maxDNSAnswers+1)
			for i := range many {
				many[i] = map[string]any{"type": "A", "value": fmt.Sprintf("10.9.0.%d", i+1), "ttl": 60}
			}
			return map[string]any{"rcode": 0, "answers": many}
		}
		return wikiAnswers(name, qtype)
	})
	other := ts.fakeNodeNamed(t, "node-b", true)
	old := privateDNSWait
	privateDNSWait = 200 * time.Millisecond
	t.Cleanup(func() { privateDNSWait = old })
	for _, c := range []struct {
		key, sb, name, qtype string
		want                 int
	}{
		{"", "sb1", "wiki.corp.internal", "A", http.StatusUnauthorized},
		{ts.s.apiKey, "sb1", "wiki.corp.internal", "A", http.StatusForbidden}, // operator key is not a node
		{other.key, "sb1", "wiki.corp.internal", "A", http.StatusForbidden},
		{node.key, "sb1", "wiki.other.internal", "A", http.StatusForbidden},
		{node.key, "sb1", "wiki.corp.internal", "AAAA", http.StatusBadRequest},
		{node.key, "sb1", strings.Repeat("a", 64) + ".corp.internal", "A", http.StatusBadRequest},
		{node.key, "sb1", strings.Repeat("a.", 127) + "corp.internal", "A", http.StatusBadRequest},
		{node.key, "sb1", "a..corp.internal", "A", http.StatusBadRequest},
		{node.key, "sb1", "wiki corp.internal", "A", http.StatusBadRequest},
		{node.key, "sb1", "", "A", http.StatusBadRequest},
		{node.key, "sb1", "slow.corp.internal", "A", http.StatusBadGateway},
		{node.key, "sb1", "big.corp.internal", "A", http.StatusBadGateway},
		{node.key, "sb1", "rcode.corp.internal", "A", http.StatusBadGateway},
	} {
		if code, _ := ts.nodeDNS(t, c.key, c.sb, c.name, c.qtype); code != c.want {
			t.Errorf("%s %s with %q: %d, want %d", c.name, c.qtype, c.key, code, c.want)
		}
	}
}

func TestDNSRelayMalformedBody(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.dnsGrant(t, wikiAnswers)
	req, _ := http.NewRequest(http.MethodPost, ts.srv.URL+"/internal/private/dns", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+node.key)
	resp, err := ts.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body: %d, want 400", resp.StatusCode)
	}
}

func TestDNSRelayConnectorOffline(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	inst := dialInstanceWith(t, ts, "prod", "inst-aaaaaaaaaaaa", routesCaps, true, wikiAnswers)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","dns":"corp.internal"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("dns grant: %d %s", resp.StatusCode, ts.body(resp))
	}
	_ = inst.ctrl.Close()
	waitFor(t, func() bool { return ts.s.connectors.count(ts.connectorID(t, "prod")) == 0 })
	if code, _ := ts.nodeDNS(t, node.key, "sb1", "wiki.corp.internal", "A"); code != http.StatusBadGateway {
		t.Fatalf("no instance online: %d, want 502", code)
	}
}

func TestDNSRelayRateLimited(t *testing.T) {
	old := privateDNSPerSecond
	privateDNSPerSecond = 2
	t.Cleanup(func() { privateDNSPerSecond = old })
	ts := newConnectorTestServer(t)
	node := ts.dnsGrant(t, wikiAnswers)
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if code, _ := ts.nodeDNS(t, node.key, "sb1", "wiki.corp.internal", "A"); code != want {
			t.Errorf("lookup %d: %d, want %d", i, code, want)
		}
	}
}

func TestDNSRelayRateLimitIsPerNode(t *testing.T) {
	old := privateDNSPerSecond
	privateDNSPerSecond = 2
	t.Cleanup(func() { privateDNSPerSecond = old })
	ts := newConnectorTestServer(t)
	node := ts.dnsGrant(t, wikiAnswers)
	other := ts.fakeNodeNamed(t, "node-b", true)
	for i, want := range []int{http.StatusForbidden, http.StatusForbidden, http.StatusTooManyRequests} {
		if code, _ := ts.nodeDNS(t, other.key, "sb1", "wiki.corp.internal", "A"); code != want {
			t.Errorf("other node lookup %d: %d, want %d", i, code, want)
		}
	}
	if code, _ := ts.nodeDNS(t, node.key, "sb1", "wiki.corp.internal", "A"); code != http.StatusOK {
		t.Fatalf("the sandbox's own node was limited by another node: %d", code)
	}
}

func TestDNSRelayLongestSuffixPicksTheConnector(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.dnsGrant(t, func(string, string) map[string]any {
		return map[string]any{"rcode": 0, "answers": []any{map[string]any{"type": "A", "value": "10.1.1.1", "ttl": 60}}}
	})
	dialInstanceWith(t, ts, "lab", "inst-bbbbbbbbbbbb", `{"targets":[],"dns":{"suffixes":["wiki.corp.internal"]}}`, true,
		func(string, string) map[string]any {
			return map[string]any{"rcode": 0, "answers": []any{map[string]any{"type": "A", "value": "10.2.2.2", "ttl": 60}}}
		})
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"lab","dns":"wiki.corp.internal"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("lab grant: %d %s", resp.StatusCode, ts.body(resp))
	}
	if _, reply := ts.nodeDNS(t, node.key, "sb1", "a.wiki.corp.internal", "A"); len(reply.Answers) != 1 || reply.Answers[0].Value != "10.2.2.2" {
		t.Fatalf("longest suffix: %+v", reply)
	}
	if _, reply := ts.nodeDNS(t, node.key, "sb1", "mail.corp.internal", "A"); len(reply.Answers) != 1 || reply.Answers[0].Value != "10.1.1.1" {
		t.Fatalf("shorter suffix: %+v", reply)
	}
}

func TestDNSLearnedKeepsAtMostTheLimit(t *testing.T) {
	t.Parallel()
	var d privateDNS
	now := time.Now()
	d.learn("sb", netip.MustParseAddr("10.0.0.1"), dnsLearned{added: now.Add(-time.Hour), expires: now.Add(-time.Minute)})
	for i := range maxDNSLearnedPerSandbox {
		a := netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)})
		d.learn("sb", a, dnsLearned{added: now.Add(time.Duration(i) * time.Millisecond), expires: now.Add(time.Minute)})
	}
	if _, ok := d.lookup("sb", netip.MustParseAddr("10.0.0.1"), now); ok {
		t.Fatal("an expired entry was kept")
	}
	d.learn("sb", netip.MustParseAddr("10.2.0.1"), dnsLearned{added: now.Add(time.Hour), expires: now.Add(time.Minute)})
	if _, ok := d.lookup("sb", netip.AddrFrom4([4]byte{10, 1, 0, 0}), now); ok {
		t.Fatal("the oldest live entry was kept past the limit")
	}
	if _, ok := d.lookup("sb", netip.MustParseAddr("10.2.0.1"), now); !ok {
		t.Fatal("the newest entry was dropped")
	}
	if n := len(d.learned["sb"]); n != maxDNSLearnedPerSandbox {
		t.Fatalf("kept %d entries, want %d", n, maxDNSLearnedPerSandbox)
	}
	d.learn("sb2", netip.MustParseAddr("10.3.0.1"), dnsLearned{added: now, expires: now.Add(-time.Second)})
	d.learn("sb2", netip.MustParseAddr("10.3.0.2"), dnsLearned{added: now, expires: now.Add(time.Minute)})
	if n := len(d.learned["sb2"]); n != 1 {
		t.Fatalf("learn kept %d entries below the limit, want the expired one swept", n)
	}
	if clampTTL(5) != 30*time.Second || clampTTL(3600) != 300*time.Second || clampTTL(120) != 120*time.Second {
		t.Fatal("clampTTL")
	}
}
