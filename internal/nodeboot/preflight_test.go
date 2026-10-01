package nodeboot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// RunPreflight only calls Name() and type-asserts IsolationChecker, so the
// fakes stub the rest of rt.Runtime just enough to satisfy the interface.
type fakeRuntime struct{ name string }

func (f fakeRuntime) Name() string          { return f.name }
func (f fakeRuntime) Capabilities() rt.Caps { return rt.Caps{} }
func (f fakeRuntime) Prepare(context.Context, rt.Template) error {
	return errors.New("not implemented in fake")
}
func (f fakeRuntime) Boot(context.Context, rt.BootSpec) (rt.Instance, error) {
	return nil, errors.New("not implemented in fake")
}
func (f fakeRuntime) Restore(context.Context, rt.SnapshotRef, rt.BootSpec) (rt.Instance, error) {
	return nil, errors.New("not implemented in fake")
}
func (f fakeRuntime) RecoverOrphans() []rt.Instance { return nil }

// fakeProcessRuntime does not implement IsolationChecker.
type fakeProcessRuntime struct{ fakeRuntime }

// fakeBareRuntime does not implement IsolationChecker either; used with
// isolation=true to exercise the fail-closed branch.
type fakeBareRuntime struct{ fakeRuntime }

// fakeIsolatingRuntime implements IsolationChecker, returning err.
type fakeIsolatingRuntime struct {
	fakeRuntime
	err error
}

func (f fakeIsolatingRuntime) CheckIsolation(context.Context) error { return f.err }

func TestRunPreflight(t *testing.T) {
	cases := []struct {
		name       string
		isolation  bool
		runtime    rt.Runtime
		egressErr  error
		wantOK     bool
		wantFailCt int
	}{
		{
			name:       "all pass",
			isolation:  true,
			runtime:    fakeIsolatingRuntime{fakeRuntime: fakeRuntime{name: "firecracker"}},
			egressErr:  nil,
			wantOK:     true,
			wantFailCt: 0,
		},
		{
			name:       "not isolating (H10 case)",
			isolation:  false,
			runtime:    fakeProcessRuntime{fakeRuntime{name: "process"}},
			egressErr:  nil,
			wantOK:     false,
			wantFailCt: 1,
		},
		{
			name:       "isolating but no IsolationChecker impl",
			isolation:  true,
			runtime:    fakeBareRuntime{fakeRuntime{name: "bare"}},
			egressErr:  nil,
			wantOK:     false,
			wantFailCt: 1,
		},
		{
			name:       "egress verify fails",
			isolation:  true,
			runtime:    fakeIsolatingRuntime{fakeRuntime: fakeRuntime{name: "firecracker"}},
			egressErr:  errors.New("DROP rule missing"),
			wantOK:     false,
			wantFailCt: 1,
		},
		{
			name:      "isolation checker AND egress both fail",
			isolation: true,
			runtime: fakeIsolatingRuntime{
				fakeRuntime: fakeRuntime{name: "firecracker"},
				err:         errors.New("tap not isolated"),
			},
			egressErr:  errors.New("x"),
			wantOK:     false,
			wantFailCt: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			egressVerify := func(mode string) error { return tc.egressErr }
			got := RunPreflight(context.Background(), tc.runtime, tc.isolation, egressVerify)
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (failures: %v)", got.OK, tc.wantOK, got.Failures)
			}
			if len(got.Failures) != tc.wantFailCt {
				t.Errorf("len(Failures) = %d, want %d (failures: %v)", len(got.Failures), tc.wantFailCt, got.Failures)
			}
		})
	}
}

// captureLogs swaps the default slog logger for one writing into a buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A passing node and a node that never ran preflight must not produce identical journals.
func TestReportPreflightLogsPasses(t *testing.T) {
	buf := captureLogs(t)
	pf := RunPreflight(context.Background(),
		fakeIsolatingRuntime{fakeRuntime: fakeRuntime{name: "firecracker"}}, true,
		func(string) error { return nil })
	if !pf.OK {
		t.Fatalf("fixture should pass: %v", pf.Failures)
	}
	if got := ReportPreflight(pf, "firecracker"); got {
		t.Errorf("failClosed = true on a passing preflight")
	}
	out := buf.String()
	for _, want := range []string{"preflight passed", "egress-rules(firecracker)", "runtime-self-check"} {
		if !strings.Contains(out, want) {
			t.Errorf("success log missing %q; got:\n%s", want, out)
		}
	}
	if len(pf.Checks) == 0 {
		t.Fatal("RunPreflight recorded no checks, so there is nothing a success log could report")
	}
	for _, c := range pf.Checks {
		if !strings.Contains(out, c.Name) {
			t.Errorf("probe %q ran but was never logged", c.Name)
		}
	}
}

func TestReportPreflightEnforcesByDefault(t *testing.T) {
	failing := PreflightResult{
		OK:       false,
		Checks:   []PreflightCheck{{Name: "egress-rules(firecracker)", Err: errors.New("10/8 DROP missing")}},
		Failures: []string{"10/8 DROP missing"},
	}
	cases := []struct {
		name, env      string
		set            bool
		wantFailClosed bool
		wantLog        string
	}{
		{name: "unset enforces", set: false, wantFailClosed: true, wantLog: "will NOT accept sandbox creates"},
		{name: "explicit 1 enforces", env: "1", set: true, wantFailClosed: true, wantLog: "will NOT accept sandbox creates"},
		{name: "explicit 0 opts out loudly", env: "0", set: true, wantFailClosed: false, wantLog: "SANDBOXD_PREFLIGHT_ENFORCE=0"},
		// A typo in the escape hatch must not silently disable the control.
		{name: "unparseable value still enforces", env: "maybe", set: true, wantFailClosed: true, wantLog: "will NOT accept sandbox creates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SANDBOXD_PREFLIGHT_ENFORCE", tc.env)
			} else {
				os.Unsetenv("SANDBOXD_PREFLIGHT_ENFORCE") //nolint:errcheck // unset of a possibly-absent var
			}
			buf := captureLogs(t)
			if got := ReportPreflight(failing, "firecracker"); got != tc.wantFailClosed {
				t.Errorf("failClosed = %v, want %v", got, tc.wantFailClosed)
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log missing %q; got:\n%s", tc.wantLog, buf.String())
			}
		})
	}
}

// fakeIptables writes a stub iptables that serves -S <CHAIN> from files.
func fakeIptables(t *testing.T, chains map[string]string) (binDir, fixDir string) {
	t.Helper()
	root := t.TempDir()
	binDir, fixDir = filepath.Join(root, "bin"), filepath.Join(root, "fix")
	for _, d := range []string{binDir, fixDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for chain, body := range chains {
		if err := os.WriteFile(filepath.Join(fixDir, chain), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\n" +
		"[ \"$1\" = \"-S\" ] || { echo \"stub iptables: unsupported: $*\" >&2; exit 2; }\n" +
		"c=${2:-FORWARD}\n" +
		"[ -f \"$FIXDIR/$c\" ] || { echo \"stub iptables: no chain $c\" >&2; exit 1; }\n" +
		"cat \"$FIXDIR/$c\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "iptables"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, fixDir
}

// runVerifyEgress runs the embedded script against a stubbed chain layout.
func runVerifyEgress(t *testing.T, chains map[string]string, mode string) (string, error) {
	t.Helper()
	binDir, fixDir := fakeIptables(t, chains)
	script := filepath.Join(t.TempDir(), "verify-egress.sh")
	if err := os.WriteFile(script, verifyEgressScript, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, mode)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "FIXDIR="+fixDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// healthyForward is copied verbatim from iptables -S on a real node.
const healthyForward = `-P FORWARD ACCEPT
-A FORWARD -j DOCKER-USER
-A FORWARD -j DOCKER-FORWARD
-A FORWARD -s 192.168.0.0/16 -d 100.64.0.0/10 -j DROP
-A FORWARD -s 192.168.0.0/16 -d 172.16.0.0/12 -j DROP
-A FORWARD -s 192.168.0.0/16 -d 10.0.0.0/8 -j DROP
-A FORWARD -d 192.168.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A FORWARD -s 192.168.0.0/16 -d 192.168.0.0/16 -j DROP
-A FORWARD -s 192.168.0.0/16 -d 169.254.0.0/16 -j DROP
-A FORWARD -s 192.168.0.0/16 -p udp -m udp --dport 53 -j ACCEPT
-A FORWARD -s 192.168.0.0/16 -p tcp -m tcp --dport 53 -j ACCEPT
-A FORWARD -s 192.168.0.0/16 -p tcp -m tcp --dport 80 -j ACCEPT
-A FORWARD -s 192.168.0.0/16 -p tcp -m tcp --dport 443 -j ACCEPT
-A FORWARD -s 192.168.0.0/16 -j DROP
`

const healthyInput = `-P INPUT ACCEPT
-A INPUT -s 192.168.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A INPUT -s 192.168.0.0/16 -j DROP
-A INPUT -s 172.18.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A INPUT -s 172.18.0.0/16 -j DROP
`

const healthyDockerUser = `-N DOCKER-USER
-A DOCKER-USER -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A DOCKER-USER -d 169.254.0.0/16 -j DROP
-A DOCKER-USER -d 10.0.0.0/8 -j DROP
-A DOCKER-USER -d 172.17.0.1/32 -j DROP
-A DOCKER-USER -d 172.16.0.0/12 -j DROP
-A DOCKER-USER -d 100.64.0.0/10 -j DROP
-A DOCKER-USER -s 172.18.0.0/16 -d 192.168.0.0/16 -j DROP
-A DOCKER-USER -s 172.18.0.0/16 -p udp -m udp --dport 53 -j RETURN
-A DOCKER-USER -s 172.18.0.0/16 -p tcp -m tcp --dport 53 -j RETURN
-A DOCKER-USER -s 172.18.0.0/16 -p tcp -m tcp --dport 80 -j RETURN
-A DOCKER-USER -s 172.18.0.0/16 -p tcp -m tcp --dport 443 -j RETURN
-A DOCKER-USER -s 172.18.0.0/16 -j DROP
`

func healthyChains() map[string]string {
	return map[string]string{"FORWARD": healthyForward, "INPUT": healthyInput, "DOCKER-USER": healthyDockerUser}
}

// Each broken chain must go red; the healthy cases must not, or enforce takes working nodes out.
func TestVerifyEgressScriptMutations(t *testing.T) {
	belowAllowlist := strings.Replace(healthyForward,
		"-A FORWARD -s 192.168.0.0/16 -d 10.0.0.0/8 -j DROP\n", "", 1)
	belowAllowlist = strings.Replace(belowAllowlist,
		"-A FORWARD -s 192.168.0.0/16 -p tcp -m tcp --dport 443 -j ACCEPT\n",
		"-A FORWARD -s 192.168.0.0/16 -p tcp -m tcp --dport 443 -j ACCEPT\n"+
			"-A FORWARD -s 192.168.0.0/16 -d 10.0.0.0/8 -j DROP\n", 1)
	if belowAllowlist == healthyForward {
		t.Fatal("the ordering mutation did not apply — a green result in a red coat")
	}

	cases := []struct {
		name     string
		mutate   func(map[string]string)
		mode     string
		wantFail bool
		wantMsg  string
	}{
		{name: "healthy firecracker", mutate: func(map[string]string) {}, mode: "--firecracker"},
		{name: "healthy docker", mutate: func(map[string]string) {}, mode: "--docker"},
		{
			name:     "10/8 DROP present but below the allowlist ACCEPT",
			mutate:   func(c map[string]string) { c["FORWARD"] = belowAllowlist },
			mode:     "--firecracker",
			wantFail: true, wantMsg: "BELOW the VM allowlist ACCEPT",
		},
		{
			name: "172.16/12 FORWARD DROP missing",
			mutate: func(c map[string]string) {
				c["FORWARD"] = strings.Replace(c["FORWARD"], "-A FORWARD -s 192.168.0.0/16 -d 172.16.0.0/12 -j DROP\n", "", 1)
			},
			mode: "--firecracker", wantFail: true, wantMsg: "172.16.0.0/12 FORWARD DROP missing",
		},
		{
			name: "100.64/10 FORWARD DROP missing",
			mutate: func(c map[string]string) {
				c["FORWARD"] = strings.Replace(c["FORWARD"], "-A FORWARD -s 192.168.0.0/16 -d 100.64.0.0/10 -j DROP\n", "", 1)
			},
			mode: "--firecracker", wantFail: true, wantMsg: "100.64.0.0/10 FORWARD DROP missing",
		},
		{
			name: "FORWARD flushed, as after a reboot with no restore unit",
			mutate: func(c map[string]string) {
				c["FORWARD"] = "-P FORWARD ACCEPT\n-A FORWARD -j DOCKER-USER\n"
			},
			mode: "--firecracker", wantFail: true, wantMsg: "FORWARD DROP missing",
		},
		{
			name: "blanket ACCEPT above the DOCKER-USER jump",
			mutate: func(c map[string]string) {
				c["FORWARD"] = "-P FORWARD ACCEPT\n-A FORWARD -i eth0 -j ACCEPT\n" +
					strings.TrimPrefix(c["FORWARD"], "-P FORWARD ACCEPT\n")
			},
			mode: "--docker", wantFail: true, wantMsg: "bypass every DOCKER-USER DROP",
		},
		{
			// A docker sandbox reached the orchestrator, Postgres and MinIO on a
			// peer host through this node's address, which their firewall trusts.
			name: "docker sandbox port allowlist missing",
			mutate: func(c map[string]string) {
				c["DOCKER-USER"] = strings.Replace(c["DOCKER-USER"], "-A DOCKER-USER -s 172.18.0.0/16 -j DROP\n", "", 1)
			},
			mode: "--docker", wantFail: true, wantMsg: "port allowlist missing",
		},
		{
			name: "docker 192.168/16 DROP missing",
			mutate: func(c map[string]string) {
				c["DOCKER-USER"] = strings.Replace(c["DOCKER-USER"], "-A DOCKER-USER -s 172.18.0.0/16 -d 192.168.0.0/16 -j DROP\n", "", 1)
			},
			mode: "--docker", wantFail: true, wantMsg: "192.168/16 DROP missing",
		},
		{
			name: "docker 10/8 DROP below the sandbox allowlist",
			mutate: func(c map[string]string) {
				c["DOCKER-USER"] = strings.Replace(c["DOCKER-USER"], "-A DOCKER-USER -d 10.0.0.0/8 -j DROP\n", "", 1) +
					"-A DOCKER-USER -d 10.0.0.0/8 -j DROP\n"
			},
			mode: "--docker", wantFail: true, wantMsg: "BELOW the sandbox allowlist",
		},
		{
			name: "FC INPUT reply-ACCEPT below the DROP",
			mutate: func(c map[string]string) {
				c["INPUT"] = "-P INPUT ACCEPT\n" +
					"-A INPUT -s 192.168.0.0/16 -j DROP\n" +
					"-A INPUT -s 192.168.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n"
			},
			mode: "--firecracker", wantFail: true, wantMsg: "is BELOW the DROP",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chains := healthyChains()
			tc.mutate(chains)
			out, err := runVerifyEgress(t, chains, tc.mode)
			if tc.wantFail && err == nil {
				t.Fatalf("script PASSED a broken chain — it is checking nothing here.\noutput:\n%s", out)
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("script failed a healthy host (%v) — over-strict checks de-advertise working nodes under enforce.\noutput:\n%s", err, out)
			}
			if tc.wantMsg != "" && !strings.Contains(out, tc.wantMsg) {
				t.Errorf("failure message does not name the problem: want %q, got:\n%s", tc.wantMsg, out)
			}
		})
	}
}

func TestVerifyEgressScriptMatchesCanonical(t *testing.T) {
	canonical, err := os.ReadFile("../../scripts/verify-egress.sh")
	if err != nil {
		t.Fatalf("read canonical script: %v", err)
	}
	if !bytes.Equal(canonical, verifyEgressScript) {
		t.Fatal("internal/nodeboot/scripts/verify-egress.sh has drifted from " +
			"scripts/verify-egress.sh — copy the canonical file over the embedded one")
	}
}
