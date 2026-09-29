package nodeboot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestNetNSIsTheDefault(t *testing.T) {
	t.Setenv("FC_NETNS", "")
	if !NetNSRequested() {
		t.Fatal("FC_NETNS unset must mean per-sandbox network namespaces.\n\n" +
			"With this false a fresh node boots on the shared bridge: per-sandbox " +
			"host-visible L2, a host-script-owned firewall, and the FDB flood surface " +
			"— the state add-sandbox-netns exists to retire.")
	}
}

func TestNetNSEscapeHatch(t *testing.T) {
	for _, off := range []string{"0", "false", "no", "off"} {
		t.Run(off, func(t *testing.T) {
			t.Setenv("FC_NETNS", off)
			if NetNSRequested() {
				t.Errorf("FC_NETNS=%q did not select the shared-bridge fallback", off)
			}
		})
	}
	for _, on := range []string{"1", "true", "yes", "on"} {
		t.Run(on, func(t *testing.T) {
			t.Setenv("FC_NETNS", on)
			if !NetNSRequested() {
				t.Errorf("FC_NETNS=%q did not select the netns path", on)
			}
		})
	}
}

// A default the boot path does not read is not a default.
func TestSelectRuntimeWiresNetNSDefault(t *testing.T) {
	body, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	if !regexp.MustCompile(`NetNS:\s*NetNSRequested\(\)`).Match(body) {
		t.Fatal("runtime.go does not set NetNS from NetNSRequested().\n\n" +
			"The default must have exactly one home. If the firecracker config reads " +
			"FC_NETNS through some other expression, the tests above assert a function " +
			"nothing consults.")
	}
}

// fakeNetNSRuntime is an isolating runtime that also reports its netns mode.
type fakeNetNSRuntime struct {
	fakeIsolatingRuntime
	netns bool
}

func (f fakeNetNSRuntime) NetNSMode() bool { return f.netns }

// Probing the wrong mode's rules under enforce-by-default de-advertises a healthy node.
func TestRunPreflightPicksEgressModeByRuntime(t *testing.T) {
	cases := []struct {
		name     string
		runtime  rt.Runtime
		wantMode string
	}{
		{
			name:     "netns firecracker verifies --netns",
			runtime:  fakeNetNSRuntime{fakeIsolatingRuntime{fakeRuntime: fakeRuntime{name: "firecracker"}, err: nil}, true},
			wantMode: "--netns",
		},
		{
			name:     "bridge firecracker verifies --firecracker",
			runtime:  fakeNetNSRuntime{fakeIsolatingRuntime{fakeRuntime: fakeRuntime{name: "firecracker"}, err: nil}, false},
			wantMode: "--firecracker",
		},
		{
			name:     "docker verifies --docker",
			runtime:  fakeIsolatingRuntime{fakeRuntime: fakeRuntime{name: "docker"}},
			wantMode: "--docker",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			verify := func(mode string) error { got = append(got, mode); return nil }
			pf := RunPreflight(context.Background(), tc.runtime, true, verify)
			if !pf.OK {
				t.Fatalf("healthy fakes must pass preflight; failures: %v", pf.Failures)
			}
			if len(got) != 1 || got[0] != tc.wantMode {
				t.Fatalf("egressVerify called with %v, want exactly [%s]", got, tc.wantMode)
			}
		})
	}
}

// runVerifyEgressNetNS runs the embedded script's --netns branch against
// stubbed nft, sysctl and iptables on PATH. table == "" means no sandboxd_netns
// table; chains is the iptables -S output per chain, an absent chain erroring
// like the real tool.
func runVerifyEgressNetNS(t *testing.T, table, ipForward string, chains map[string]string) (string, error) {
	t.Helper()
	root := t.TempDir()
	binDir, fixDir := filepath.Join(root, "bin"), filepath.Join(root, "fix")
	for _, d := range []string{binDir, fixDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if table != "" {
		if err := os.WriteFile(filepath.Join(fixDir, "nft-table"), []byte(table), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nftStub := "#!/bin/sh\n" +
		"[ \"$*\" = \"list table ip sandboxd_netns\" ] || { echo \"stub nft: unsupported: $*\" >&2; exit 2; }\n" +
		"[ -f \"$FIXDIR/nft-table\" ] || { echo \"Error: No such file or directory\" >&2; exit 1; }\n" +
		"cat \"$FIXDIR/nft-table\"\n"
	sysctlStub := "#!/bin/sh\n" +
		"[ \"$*\" = \"-n net.ipv4.ip_forward\" ] || { echo \"stub sysctl: unsupported: $*\" >&2; exit 2; }\n" +
		"printf '%s\\n' \"$IPFORWARD\"\n"
	iptablesStub := "#!/bin/sh\n" +
		"[ \"$1\" = \"-S\" ] || { echo \"stub iptables: unsupported: $*\" >&2; exit 2; }\n" +
		"c=${2:-FORWARD}\n" +
		"[ -f \"$FIXDIR/ipt-$c\" ] || { echo \"iptables: No chain/target/match by that name.\" >&2; exit 1; }\n" +
		"cat \"$FIXDIR/ipt-$c\"\n"
	for chain, body := range chains {
		if err := os.WriteFile(filepath.Join(fixDir, "ipt-"+chain), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"nft": nftStub, "sysctl": sysctlStub, "iptables": iptablesStub} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(t.TempDir(), "verify-egress.sh")
	if err := os.WriteFile(script, verifyEgressScript, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, "--netns")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"), "FIXDIR="+fixDir, "IPFORWARD="+ipForward)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// healthyNetNSTable is copied verbatim from nft output on a real node, since
// the checks grep what the tool prints, not what was installed.
const healthyNetNSTable = `table ip sandboxd_netns {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr 10.128.0.0/16 oifname "eno1" masquerade
	}

	chain input {
		type filter hook input priority filter; policy accept;
		ip saddr 10.128.0.0/16 ct state established,related accept
		iifname "vh*" ip saddr 10.128.0.0/16 drop
	}

	chain forward {
		type filter hook forward priority filter; policy accept;
		ip saddr 10.128.0.0/16 ip daddr 10.128.0.0/16 drop
		ip saddr 10.128.0.0/16 ip daddr 169.254.0.0/16 drop
	}
}
`

// healthyBaseChains is a node that has docker — so FORWARD's policy is DROP —
// with the accepts EnsureNetNSHostRules installs. Copied from iptables -S on a
// real node; the FORWARD half is verbatim what a provisioned node prints.
var healthyBaseChains = map[string]string{
	"FORWARD": `-P FORWARD DROP
-A FORWARD -j DOCKER-USER
-A FORWARD -j DOCKER-FORWARD
-A FORWARD -s 192.168.0.0/16 -d 169.254.0.0/16 -j DROP
-A FORWARD -d 192.168.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A FORWARD -s 192.168.0.0/16 -j DROP
`,
	"DOCKER-USER": `-N DOCKER-USER
-A DOCKER-USER -d 10.128.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A DOCKER-USER -s 10.128.0.0/16 -j ACCEPT
-A DOCKER-USER -d 169.254.0.0/16 -j DROP
-A DOCKER-USER -d 10.0.0.0/8 -j DROP
`,
}

// baseChainsWithout drops one rule from DOCKER-USER, the way a node that never
// ran the fix — or one whose iptables call failed — actually looks.
func baseChainsWithout(t *testing.T, rule string) map[string]string {
	t.Helper()
	du := strings.Replace(healthyBaseChains["DOCKER-USER"], rule+"\n", "", 1)
	if du == healthyBaseChains["DOCKER-USER"] {
		t.Fatalf("mutation did not apply: %q not in the fixture", rule)
	}
	return map[string]string{"FORWARD": healthyBaseChains["FORWARD"], "DOCKER-USER": du}
}

func TestVerifyEgressNetNSMutations(t *testing.T) {
	del := func(line string) string {
		out := strings.Replace(healthyNetNSTable, line+"\n", "", 1)
		if out == healthyNetNSTable {
			t.Fatalf("mutation did not apply: %q not found", line)
		}
		return out
	}
	swapInput := strings.Replace(healthyNetNSTable,
		"\t\tip saddr 10.128.0.0/16 ct state established,related accept\n"+
			"\t\tiifname \"vh*\" ip saddr 10.128.0.0/16 drop\n",
		"\t\tiifname \"vh*\" ip saddr 10.128.0.0/16 drop\n"+
			"\t\tip saddr 10.128.0.0/16 ct state established,related accept\n", 1)
	if swapInput == healthyNetNSTable {
		t.Fatal("the input-ordering mutation did not apply")
	}

	cases := []struct {
		name     string
		table    string
		ipf      string
		chains   map[string]string // nil means a host with no iptables chains at all
		wantFail bool
		wantMsg  string
	}{
		{name: "healthy", table: healthyNetNSTable, ipf: "1", chains: healthyBaseChains},
		{
			// The defect measured on a real node 2026-09-22: sandboxd_netns is
			// perfect and the sandbox has no outbound network at all, because
			// installing docker set FORWARD's policy to DROP and every accept
			// the host setup installs names the old bridge subnet.
			name: "FORWARD drops and nothing accepts the slot supernet", table: healthyNetNSTable, ipf: "1",
			chains: map[string]string{
				"FORWARD":     healthyBaseChains["FORWARD"],
				"DOCKER-USER": "-N DOCKER-USER\n-A DOCKER-USER -d 169.254.0.0/16 -j DROP\n",
			},
			wantFail: true, wantMsg: "NO outbound network at all",
		},
		{
			name: "egress accepted, replies not", table: healthyNetNSTable, ipf: "1",
			chains:   baseChainsWithout(t, "-A DOCKER-USER -d 10.128.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"),
			wantFail: true, wantMsg: "no reply is forwarded back",
		},
		{
			// A node with no docker keeps FORWARD's default ACCEPT policy, so
			// there is nothing for the accepts to survive and demanding them
			// would de-advertise a working node.
			name: "FORWARD policy ACCEPT needs no exception", table: healthyNetNSTable, ipf: "1",
			chains: map[string]string{"FORWARD": "-P FORWARD ACCEPT\n-A FORWARD -s 192.168.0.0/16 -j DROP\n"},
		},
		{
			// The shape emitted when no default route resolves.
			name:  "healthy without oifname scope",
			table: strings.Replace(healthyNetNSTable, " oifname \"eno1\"", "", 1),
			ipf:   "1",
		},
		{
			name: "table missing entirely", table: "", ipf: "1",
			wantFail: true, wantMsg: "sandboxd_netns missing",
		},
		{
			name: "ip_forward off", table: healthyNetNSTable, ipf: "0",
			wantFail: true, wantMsg: "ip_forward is 0",
		},
		{
			name: "masquerade missing", ipf: "1",
			table:    del("\t\tip saddr 10.128.0.0/16 oifname \"eno1\" masquerade"),
			wantFail: true, wantMsg: "no masquerade",
		},
		{
			name: "vh* input drop missing", ipf: "1",
			table:    del("\t\tiifname \"vh*\" ip saddr 10.128.0.0/16 drop"),
			wantFail: true, wantMsg: "input drop",
		},
		{
			name: "established accept missing", ipf: "1",
			table:    del("\t\tip saddr 10.128.0.0/16 ct state established,related accept"),
			wantFail: true, wantMsg: "established,related accept missing",
		},
		{
			name: "accept below the drop", table: swapInput, ipf: "1",
			wantFail: true, wantMsg: "BELOW the vh* drop",
		},
		{
			name: "slot-to-slot forward drop missing", ipf: "1",
			table:    del("\t\tip saddr 10.128.0.0/16 ip daddr 10.128.0.0/16 drop"),
			wantFail: true, wantMsg: "slot-to-slot forward drop missing",
		},
		{
			name: "metadata forward drop missing", ipf: "1",
			table:    del("\t\tip saddr 10.128.0.0/16 ip daddr 169.254.0.0/16 drop"),
			wantFail: true, wantMsg: "metadata forward drop missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runVerifyEgressNetNS(t, tc.table, tc.ipf, tc.chains)
			if tc.wantFail && err == nil {
				t.Fatalf("script PASSED a broken netns host — it is checking nothing here.\noutput:\n%s", out)
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("script failed a healthy netns host (%v) — over-strict checks de-advertise working nodes under enforce.\noutput:\n%s", err, out)
			}
			if tc.wantMsg != "" && !strings.Contains(out, tc.wantMsg) {
				t.Errorf("failure message does not name the problem: want %q, got:\n%s", tc.wantMsg, out)
			}
		})
	}
}
