package httpapi

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// scripts/security-suite.sh reads the node's unenforced-cap counter from
// /metrics, which a tenant key is refused on; a refusal must read as SKIP,
// never as the FAIL that means the cap is silently ineffective.
func TestSecuritySuiteDiskVerdictNeedsAnOperatorKey(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	srv, st, tenant := newTestServerWithStore(t)
	_, operator, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	verdict := func(operatorKey string) string {
		cmd := exec.Command("bash", "../../scripts/security-suite.sh", "--host", srv.URL)
		cmd.Env = append(os.Environ(), "SELFTEST_DISK_WROTE=3000000000",
			"PILLDROPS_API_KEY="+tenant, "PILLDROPS_OPERATOR_KEY="+operatorKey)
		out, _ := cmd.CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "RESULT|") {
				return strings.Split(line, "|")[1]
			}
		}
		t.Fatalf("no verdict:\n%s", out)
		return ""
	}
	if got := verdict(""); got != "SKIP:blocked" {
		t.Errorf("no operator key: verdict %s, want SKIP:blocked", got)
	}
	if got := verdict(tenant); got != "SKIP:blocked" {
		t.Errorf("a tenant key as the operator key: verdict %s, want SKIP:blocked", got)
	}
	// This node enforces nothing and reports a zero counter, so reading
	// /metrics reaches the real verdict: over the cap and undetected.
	if got := verdict(operator); got != "FAIL" {
		t.Errorf("operator key: verdict %s, want FAIL from the counter it read", got)
	}
}
