package agentd

import (
	"strings"
	"sync"
	"testing"
)

func TestRegisteredSecretIsReplacedByName(t *testing.T) {
	r := NewRedactor()
	r.AddSecret("ANTHROPIC_API_KEY", "topsecretkeyvalue123")

	got := r.Redact("running with topsecretkeyvalue123 in the env")
	if strings.Contains(got, "topsecretkeyvalue123") {
		t.Fatalf("secret survived: %q", got)
	}
	if !strings.Contains(got, "[redacted:ANTHROPIC_API_KEY]") {
		t.Errorf("no named marker in %q", got)
	}
}

func TestEveryOccurrenceIsReplaced(t *testing.T) {
	r := NewRedactor()
	r.AddSecret("KEY", "abcdefgh12345678")
	got := r.Redact("abcdefgh12345678 then abcdefgh12345678 and abcdefgh12345678")
	if strings.Contains(got, "abcdefgh12345678") {
		t.Errorf("a later occurrence survived: %q", got)
	}
}

func TestShortValuesAreNotTreatedAsSecrets(t *testing.T) {
	r := NewRedactor()
	for _, v := range []string{"", "1", "true", "abc", "short"} {
		r.AddSecret("X", v)
	}
	in := "the build is true and step 1 of abc is short"
	if got := r.Redact(in); got != in {
		t.Errorf("short value corrupted ordinary text:\n in: %q\nout: %q", in, got)
	}
}

func TestOverlappingSecretsAreReplacedLongestFirst(t *testing.T) {
	r := NewRedactor()
	r.AddSecret("SHORT", "prefix12345678")
	r.AddSecret("LONG", "prefix12345678suffix87654321")

	got := r.Redact("value prefix12345678suffix87654321 end")
	if strings.Contains(got, "suffix87654321") {
		t.Errorf("fragment of the longer secret survived: %q", got)
	}
	if !strings.Contains(got, "[redacted:LONG]") {
		t.Errorf("longer secret was not matched as a whole: %q", got)
	}
}

func TestCredentialShapesAreRedacted(t *testing.T) {
	r := NewRedactor()
	for name, in := range map[string]string{
		"anthropic":  "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"openai":     "sk-proj-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"xai":        "xai-CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
		"github pat": "ghp_DDDDDDDDDDDDDDDDDDDDDDDDDDDDDD",
		"github fg":  "github_pat_EEEEEEEEEEEEEEEEEEEEEEEE",
		"aws key id": "AKIAIOSFODNN7EXAMPLE",
		"slack":      "xoxb-123456789012-abcdefghijklmnop",
		"google":     "AIzaSyABCDEFGHIJKLMNOPQRSTUVWXYZ0123456",
		"jwt":        "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"our aat":    "sfa_a1b2c3d4_0123456789abcdef0123456789abcdef",
	} {
		out := r.Redact("token is " + in + " ok")
		if strings.Contains(out, in) {
			t.Errorf("%s credential not redacted: %q", name, out)
		}
		if !strings.Contains(out, patternMarker) {
			t.Errorf("%s produced no marker: %q", name, out)
		}
	}
}

func TestPrivateKeyBlockIsRedactedWhole(t *testing.T) {
	r := NewRedactor()
	pem := `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAsecretkeymaterialhere
morekeymaterialonasecondline
-----END RSA PRIVATE KEY-----`

	got := r.Redact("here is the key:\n" + pem + "\ndone")
	if strings.Contains(got, "secretkeymaterialhere") || strings.Contains(got, "morekeymaterial") {
		t.Errorf("key material survived: %q", got)
	}
	if strings.Contains(got, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("PEM header survived, so only part of the block was replaced: %q", got)
	}
}

func TestPatternsDoNotFireOnProse(t *testing.T) {
	r := NewRedactor()
	for _, in := range []string{
		"use an sk- key from the dashboard",
		"set ghp_ tokens in your environment",
		"the AKIA prefix identifies an access key",
		"tokens look like xoxb- followed by digits",
		"a JWT starts with eyJ and has three parts",
		"read the value from ANTHROPIC_API_KEY",
	} {
		if got := r.Redact(in); got != in {
			t.Errorf("prose was redacted:\n in: %q\nout: %q", in, got)
		}
	}
}

func TestNothingToRedactReturnsInputUnchanged(t *testing.T) {
	r := NewRedactor()
	r.AddSecret("KEY", "unrelatedsecretvalue")

	in := "Reading src/main.go and running the tests"
	if got := r.Redact(in); got != in {
		t.Errorf("clean text was modified: %q", got)
	}
}

func TestNilRedactorStillAppliesPatterns(t *testing.T) {
	var r *Redactor
	got := r.Redact("key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA here")
	if strings.Contains(got, "sk-ant-api03") {
		t.Errorf("nil redactor skipped pattern redaction: %q", got)
	}
}

func TestRedactorIsSafeUnderConcurrentUse(t *testing.T) {
	r := NewRedactor()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				r.AddSecret("KEY", "rotatingsecretvalue")
				if got := r.Redact("value rotatingsecretvalue here"); strings.Contains(got, "rotatingsecret") {
					t.Error("secret survived a concurrent redact")
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
