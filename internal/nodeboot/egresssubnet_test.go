package nodeboot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A dual-stack sandboxd-net lists two subnets, and the inspect template glued
// them into one unusable string ("172.30.0.0/16fd00::/64"), so every rule keyed
// on it matched nothing. The setup script must take the IPv4 one, or refuse.
func TestDockerEgressSetupKeysOnTheIPv4Subnet(t *testing.T) {
	run := func(subnets string) (string, string) {
		bin := t.TempDir()
		log := filepath.Join(bin, "iptables.log")
		stubs := map[string]string{
			"docker":    "#!/bin/sh\nprintf '" + subnets + "'\n",
			"iptables":  "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$1\" in -D) exit 1;; esac\n",
			"ip6tables": "#!/bin/sh\nexit 1\n",
		}
		for name, body := range stubs {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("bash", "../../scripts/setup-docker-egress.sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, _ := cmd.CombinedOutput() // the later iptables-save needs root; the rules come first
		rules, _ := os.ReadFile(log)
		return string(out), string(rules)
	}

	_, rules := run(`fd00::/64\n172.30.0.0/16\n`)
	if !strings.Contains(rules, "-s 172.30.0.0/16 ") {
		t.Errorf("no rule keyed on the IPv4 subnet:\n%s", rules)
	}
	if strings.Contains(rules, "fd00") {
		t.Errorf("an IPv6 subnet reached an IPv4 rule:\n%s", rules)
	}

	out, rules := run(`fd00::/64\n`)
	if !strings.Contains(out, "cannot read an IPv4 subnet") || strings.Contains(rules, "-s ") {
		t.Errorf("an IPv6-only network was not refused; output %q, rules:\n%s", out, rules)
	}
}
