package webhook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The verification samples in docs/agent/webhook-verification.md are extracted
// and run against a real signature, so the published code cannot drift from Sign.
// Skips when node or python is absent.

const (
	docSecret = "whsec_testsecret"
	docTS     = int64(1786000000)
)

var docBody = []byte(`{"event":"turn.completed","turn_id":"tn_1"}`)

// extractFence pulls the first fenced block of the given language out of the document.
func extractFence(t *testing.T, lang string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "agent", "webhook-verification.md"))
	if err != nil {
		t.Fatalf("read the doc: %v", err)
	}
	open := "```" + lang + "\n"
	i := strings.Index(string(raw), open)
	if i < 0 {
		t.Fatalf("no %s sample in the published doc — it was removed or the fence was renamed, "+
			"so nothing is checking the sample readers copy", lang)
	}
	rest := string(raw)[i+len(open):]
	j := strings.Index(rest, "```")
	if j < 0 {
		t.Fatalf("unterminated %s fence", lang)
	}
	return rest[:j]
}

func run(t *testing.T, dir, bin string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s is not installed", bin)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestNodeSampleVerifiesARealSignature(t *testing.T) {
	sample := extractFence(t, "js")
	dir := t.TempDir()

	// Convert the ES module sample to CommonJS; everything else stays byte-identical.
	body := strings.Replace(sample, "export function verify", "function verify", 1)
	body = strings.Replace(body, `import crypto from "node:crypto";`, `const crypto = require("node:crypto");`, 1)

	sig := Sign(docSecret, time.Unix(docTS, 0), docBody)
	harness := body + fmt.Sprintf(`
const secret = %q;
const rawBody = Buffer.from(%q);
const fresh = { headers: { "x-pilldrops-signature": %q, "x-pilldrops-timestamp": String(Math.floor(Date.now()/1000)) } };

// A signature over the SAME material, but stamped now, so the freshness check passes.
fresh.headers["x-pilldrops-signature"] = "sha256=" + crypto.createHmac("sha256", secret)
  .update(fresh.headers["x-pilldrops-timestamp"] + ".").update(rawBody).digest("hex");

const results = {
  valid: verify(fresh, rawBody, secret),
  stale: verify({ headers: { "x-pilldrops-signature": %q, "x-pilldrops-timestamp": String(%d) } }, rawBody, secret),
  tampered: verify(fresh, Buffer.from(rawBody.toString() + " "), secret),
  wrongSecret: verify(fresh, rawBody, "whsec_other"),
  missing: verify({ headers: {} }, rawBody, secret),
};
console.log(JSON.stringify(results));
`, docSecret, string(docBody), sig, sig, docTS)

	path := filepath.Join(dir, "sample.js")
	if err := os.WriteFile(path, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dir, "node", path)
	if err != nil {
		t.Fatalf("the published Node sample does not run: %v\n%s", err, out)
	}
	assertOutcomes(t, "Node", out)
}

func TestPythonSampleVerifiesARealSignature(t *testing.T) {
	sample := extractFence(t, "python")
	dir := t.TempDir()

	sig := Sign(docSecret, time.Unix(docTS, 0), docBody)
	harness := sample + fmt.Sprintf(`

import json
secret = %q
raw = %q.encode()

def sign(ts):
    m = hmac.new(secret.encode(), digestmod=hashlib.sha256)
    m.update(str(ts).encode()); m.update(b"."); m.update(raw)
    return "sha256=" + m.hexdigest()

now = int(time.time())
fresh = {"X-Pilldrops-Signature": sign(now), "X-Pilldrops-Timestamp": str(now)}

print(json.dumps({
    "valid": verify(fresh, raw, secret),
    "stale": verify({"X-Pilldrops-Signature": %q, "X-Pilldrops-Timestamp": "%d"}, raw, secret),
    "tampered": verify(fresh, raw + b" ", secret),
    "wrongSecret": verify(fresh, raw, "whsec_other"),
    "missing": verify({}, raw, secret),
}))
`, docSecret, string(docBody), sig, docTS)

	path := filepath.Join(dir, "sample.py")
	if err := os.WriteFile(path, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := "python3"
	out, err := run(t, dir, bin, path)
	if err != nil {
		t.Fatalf("the published Python sample does not run: %v\n%s", err, out)
	}
	assertOutcomes(t, "Python", out)
}

// assertOutcomes checks the five cases every published sample must get right.
func assertOutcomes(t *testing.T, lang, out string) {
	t.Helper()
	got := strings.TrimSpace(out)
	// Normalise Python booleans.
	got = strings.ReplaceAll(strings.ReplaceAll(got, "True", "true"), "False", "false")

	for _, c := range []struct{ key, want string }{
		{"valid", "true"},
		{"stale", "false"},
		{"tampered", "false"},
		{"wrongSecret", "false"},
		// A clean false, not an exception; a throw becomes a 500 that gets retried.
		{"missing", "false"},
	} {
		frag := fmt.Sprintf("%q:%s", c.key, c.want)
		frag2 := fmt.Sprintf("%q: %s", c.key, c.want)
		if !strings.Contains(got, frag) && !strings.Contains(got, frag2) {
			t.Errorf("%s sample: %s should be %s\ngot: %s", lang, c.key, c.want, got)
		}
	}
}
