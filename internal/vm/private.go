package vm

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

const (
	privateProxyPort = 18445
	privateSubnet    = "192.168.31.0/24"
)

// privateRoute is one granted target at its sandbox-local address. The
// 192.168.31.0/24 range exists only inside the sandbox's own namespace.
type privateRoute struct {
	Target sandbox.PrivateTarget
	IP     string
}

func assignPrivate(ts []sandbox.PrivateTarget) []privateRoute {
	sorted := append([]sandbox.PrivateTarget(nil), ts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	out := make([]privateRoute, len(sorted))
	for i, t := range sorted {
		out[i] = privateRoute{Target: t, IP: fmt.Sprintf("192.168.31.%d", i+2)}
	}
	return out
}

func privateDNS(routes []privateRoute) map[string]string {
	m := make(map[string]string, 2*len(routes))
	for _, r := range routes {
		m[r.Target.Name+".private."] = r.IP
		host := strings.ToLower(strings.TrimSuffix(r.Target.Host, "."))
		// The orchestrator refuses these too; a host here must never shadow a peer or target name.
		if host != "" && !strings.HasSuffix(host, ".pd") && !strings.HasSuffix(host, ".private") {
			m[host+"."] = r.IP
		}
	}
	return m
}

func privateRedirect(on bool, tapName string) string {
	if !on {
		return ""
	}
	return fmt.Sprintf("\t\tiifname %q ip daddr %s meta l4proto tcp redirect to :%d\n", tapName, privateSubnet, privateProxyPort)
}

func privateEgressDrop(on bool) string {
	if !on {
		return ""
	}
	return "\t\tip daddr " + privateSubnet + " drop\n"
}
