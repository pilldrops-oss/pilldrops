package nodeboot

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
			if !regexp.MustCompile(`(iptables -I|top) INPUT`).MatchString(src) {
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

// A partial earlier run (the reply-ACCEPT kept, the DROP lost) must not end
// with the DROP inserted above the ACCEPT, where it eats every reply.
func TestSetupDockerEgressRestoresTheInputOrder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the script would persist the stub's rules over this host's /etc/iptables")
	}
	bin, fix := t.TempDir(), t.TempDir()
	stubs := map[string]string{
		// Stateful: each chain is a file of rule specs, top first.
		"iptables": `#!/bin/bash
op=$1; f="$FIXDIR/$2"; shift 2; rule="$*"
case $op in
  -N) [ -f "$f" ] && exit 1; : >"$f" ;;
  -I) touch "$f"; { echo "$rule"; cat "$f"; } >"$f.new"; mv "$f.new" "$f" ;;
  -C) grep -qxF -- "$rule" "$f" 2>/dev/null ;;
  -D) grep -qxF -- "$rule" "$f" 2>/dev/null || exit 1
      awk -v r="$rule" '!d && $0 == r { d = 1; next } { print }' "$f" >"$f.new"; mv "$f.new" "$f" ;;
esac
`,
		"docker":    "#!/bin/sh\necho 172.18.0.0/16\n",
		"ip6tables": "#!/bin/sh\nexit 1\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	accept := "-s 172.18.0.0/16 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT"
	drop := "-s 172.18.0.0/16 -j DROP"
	if err := os.WriteFile(filepath.Join(fix, "INPUT"), []byte(accept+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../../scripts/setup-docker-egress.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FIXDIR="+fix)
	// Persisting to /etc/iptables needs root and comes after the rules, so its failure is not this test's.
	out, _ := cmd.CombinedOutput()
	b, err := os.ReadFile(filepath.Join(fix, "INPUT"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), accept+"\n"+drop+"\n"; got != want {
		t.Fatalf("INPUT after a re-run:\n%s\nwant:\n%s\nscript output:\n%s", got, want, out)
	}
}
