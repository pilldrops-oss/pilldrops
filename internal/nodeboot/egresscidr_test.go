package nodeboot

import (
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
)

// iptables -S masks host bits, so grepping for 192.168.30.0/16 never matches
// the installed 192.168.0.0/16 and a healthy node fails preflight.
func TestVerifyEgressCIDRsAreInTheFormIptablesPrints(t *testing.T) {
	for _, path := range []string{
		"scripts/verify-egress.sh",       // embedded copy
		"../../scripts/verify-egress.sh", // operator-facing original
	} {
		t.Run(path, func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for _, cidr := range matchedCIDRs(string(body)) {
				ip, ipnet, err := net.ParseCIDR(cidr)
				if err != nil {
					t.Errorf("%q is not a valid CIDR: %v", cidr, err)
					continue
				}
				// Address and masked network differ only when host bits are set.
				if !ip.Equal(ipnet.IP) {
					t.Errorf("verify-egress.sh matches on %q, but iptables prints %q "+
						"(host bits are masked off on insert) — this check can never match",
						cidr, ipnet.String())
				}
			}
		})
	}
}

// matchedCIDRs pulls the CIDRs out of the script's grep patterns, unescaping the dots.
func matchedCIDRs(script string) []string {
	re := regexp.MustCompile(`(\d{1,3}(?:\\?\.\d{1,3}){3}/\d{1,2})`)
	var out []string
	for _, line := range strings.Split(script, "\n") {
		t := strings.TrimSpace(line)
		// Script comments quote the wrong form on purpose; skip them.
		if strings.HasPrefix(t, "#") || !strings.Contains(t, "grep") {
			continue
		}
		for _, m := range re.FindAllString(t, -1) {
			out = append(out, strings.ReplaceAll(m, `\`, ""))
		}
	}
	return out
}

// VMs get 192.168.30.x with a /16 mask, so the VM-to-VM rule must cover the whole /16.
func TestVerifyEgressCoversTheVMSubnetActuallyInUse(t *testing.T) {
	body, err := os.ReadFile("scripts/verify-egress.sh")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, vmNet, err := net.ParseCIDR("192.168.30.1/16") // fcbr0 gateway address
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), strings.ReplaceAll(vmNet.String(), ".", `\.`)) {
		t.Errorf("verify-egress.sh does not check the VM subnet %s that fcbr0 actually serves", vmNet)
	}
}

// DOCKER-USER is only reached from FORWARD; container-to-host traffic goes
// through INPUT, so both installers need an INPUT DROP for the sandbox subnet.
func TestBothRuntimesBlockSandboxToHost(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"firecracker", "../../scripts/setup-host.sh"},
		{"docker", "../../scripts/setup-docker-egress.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			src := string(body)
			if !strings.Contains(src, "iptables -I INPUT") {
				t.Errorf("%s installs no INPUT rule: a sandbox can open connections to "+
					"services the node binds on 0.0.0.0. DOCKER-USER/FORWARD rules do not "+
					"cover container-to-host traffic.", tc.path)
			}
			// Without the ESTABLISHED accept the DROP also eats replies to node-initiated connections.
			if !strings.Contains(src, "ESTABLISHED,RELATED -j ACCEPT") {
				t.Errorf("%s has an INPUT DROP with no ESTABLISHED,RELATED ACCEPT above it — "+
					"replies to connections the NODE opened would be dropped, breaking the preview proxy", tc.path)
			}
		})
	}
}
