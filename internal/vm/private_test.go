package vm

import (
	"net"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

func TestAssignPrivateIsStable(t *testing.T) {
	t.Parallel()
	a := assignPrivate([]sandbox.PrivateTarget{{Name: "web", Port: 80}, {Name: "db", Port: 5432}})
	if len(a) != 2 || a[0].Target.Name != "db" || a[0].IP != "192.168.31.2" || a[1].IP != "192.168.31.3" {
		t.Fatalf("%+v", a)
	}
}

func TestPrivateDNSNames(t *testing.T) {
	t.Parallel()
	m := privateDNS(assignPrivate([]sandbox.PrivateTarget{{Name: "api", Host: "Internal.Corp", Port: 443}}))
	if m["api.private."] != "192.168.31.2" || m["internal.corp."] != "192.168.31.2" || len(m) != 2 {
		t.Fatalf("%v", m)
	}
}

func TestRulesetPrivateRedirectBeforeDivert(t *testing.T) {
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	rs := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false, true)
	for _, want := range []string{
		`iifname "tap0" ip daddr 192.168.31.0/24 meta l4proto tcp redirect to :18445`,
		`iifname "tap0" udp dport 53 redirect to :53`,
		`ip daddr 192.168.31.0/24 drop`,
		"ip daddr 10.0.0.0/8 drop",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("missing %q in\n%s", want, rs)
		}
	}
	if strings.Index(rs, "192.168.31.0/24 drop") > strings.Index(rs, "dport { 53, 80, 443 }") {
		t.Error("UDP to private addresses must drop before the default accepts")
	}
	off := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false, false)
	if strings.Contains(off, "192.168.31") || strings.Contains(off, ":53") {
		t.Errorf("private off still renders private rules:\n%s", off)
	}
}
