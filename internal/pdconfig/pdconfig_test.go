package pdconfig

import "testing"

var ids = []string{"playwright", "base", "node"}

func TestParseMinimal(t *testing.T) {
	c, err := Parse(`{"version":1,"template":"base"}`, ids)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Template != "base" {
		t.Fatalf("Template = %q", c.Template)
	}
}

func TestParseFull(t *testing.T) {
	raw := `{"version":1,"template":"playwright",
	         "resources":{"vcpu":2,"memory_mb":2048},
	         "setup":["npm ci"],"start":"npx playwright test","port":9323,
	         "env":["STRIPE_SECRET_KEY"]}`
	c, err := Parse(raw, ids)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Resources.VCPU != 2 || c.Resources.MemoryMB != 2048 {
		t.Errorf("resources = %+v", c.Resources)
	}
	if len(c.Setup) != 1 || c.Setup[0] != "npm ci" {
		t.Errorf("setup = %v", c.Setup)
	}
	if c.Port != 9323 || c.Start != "npx playwright test" {
		t.Errorf("start/port = %q %d", c.Start, c.Port)
	}
	if len(c.Env) != 1 || c.Env[0] != "STRIPE_SECRET_KEY" {
		t.Errorf("env = %v", c.Env)
	}
}

func TestParseMalformedNamesTheFile(t *testing.T) {
	_, err := Parse(`{not json`, ids)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), File) {
		t.Errorf("error should name %s: %v", File, err)
	}
}

func TestParseUnknownTemplateNamesTheID(t *testing.T) {
	_, err := Parse(`{"version":1,"template":"kubernetes"}`, ids)
	if err == nil || !contains(err.Error(), "kubernetes") {
		t.Fatalf("expected an error naming the id, got %v", err)
	}
}

func TestParseUnrecognisedVersion(t *testing.T) {
	if _, err := Parse(`{"version":99,"template":"base"}`, ids); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseEnvValuesRejectedWithoutEchoingTheSecret(t *testing.T) {
	_, err := Parse(`{"version":1,"template":"base","env":{"K":"sk_live_leak"}}`, ids)
	if err == nil {
		t.Fatal("expected env-as-values to be rejected")
	}
	if contains(err.Error(), "sk_live_leak") {
		t.Fatalf("error echoed the secret back: %v", err)
	}
	if !contains(err.Error(), "name") && !contains(err.Error(), "NAME") {
		t.Errorf("error should explain names-only: %v", err)
	}
}

func TestParseEnvNames(t *testing.T) {
	c, err := Parse(`{"version":1,"template":"base","env":["A","B"]}`, ids)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Env) != 2 {
		t.Fatalf("env = %v", c.Env)
	}
}

func TestFindInPrefersTheRoot(t *testing.T) {
	i, ok := FindIn([]string{"src/main.go", "vendor/x/pilldrops.json", "pilldrops.json"})
	if !ok || i != 2 {
		t.Fatalf("FindIn = %d,%v — want the ROOT file", i, ok)
	}
}

func TestFindInIgnoresNestedOnly(t *testing.T) {
	if _, ok := FindIn([]string{"vendor/x/pilldrops.json"}); ok {
		t.Fatal("a nested-only config must be ignored")
	}
}

func TestFindInAbsent(t *testing.T) {
	if _, ok := FindIn([]string{"main.go"}); ok {
		t.Fatal("expected not found")
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
