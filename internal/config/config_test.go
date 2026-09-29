package config

import (
	"strings"
	"testing"
	"time"
)

func TestInvalidValueIsRecordedNotSwallowed(t *testing.T) {
	Reset()
	t.Setenv("PILLDROPS_IMAGE_REQUIRE_DIGEST", "true")

	if got := Int("PILLDROPS_IMAGE_REQUIRE_DIGEST", 0); got != 0 {
		t.Fatalf("Int = %d, want the default 0", got)
	}
	bad := Invalid()
	if len(bad) != 1 || bad[0].Key != "PILLDROPS_IMAGE_REQUIRE_DIGEST" {
		t.Fatalf("Invalid() = %v, want the one bad key", bad)
	}
	if err := Validate(); err == nil {
		t.Fatal("Validate() = nil; a present-but-unparseable value must fail startup")
	} else if !strings.Contains(err.Error(), "PILLDROPS_IMAGE_REQUIRE_DIGEST") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

func TestBoolAcceptsRealSpellings(t *testing.T) {
	for _, raw := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		Reset()
		t.Setenv("FLAG", raw)
		if !Bool("FLAG", false) {
			t.Errorf("Bool(%q) = false, want true", raw)
		}
		if len(Invalid()) != 0 {
			t.Errorf("Bool(%q) recorded as invalid", raw)
		}
	}
	for _, raw := range []string{"0", "false", "no", "off"} {
		Reset()
		t.Setenv("FLAG", raw)
		if Bool("FLAG", true) {
			t.Errorf("Bool(%q) = true, want false", raw)
		}
	}
	Reset()
	t.Setenv("FLAG", "maybe")
	if Bool("FLAG", false) {
		t.Error("an unparseable flag must fall back to its default")
	}
	if len(Invalid()) != 1 {
		t.Error("an unparseable flag must be recorded as invalid")
	}
}

func TestUnsetAndValidValuesAreClean(t *testing.T) {
	Reset()
	t.Setenv("PORT_NUM", "9090")
	if got := Int("PORT_NUM", 8080); got != 9090 {
		t.Errorf("Int = %d, want 9090", got)
	}
	if got := Int("ABSENT_NUM", 42); got != 42 {
		t.Errorf("Int(unset) = %d, want the default 42", got)
	}
	if got := Duration("ABSENT_DUR", 30*time.Second); got != 30*time.Second {
		t.Errorf("Duration(unset) = %v", got)
	}
	if err := Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	Reset()
	t.Setenv("ORCHESTRATOR_API_KEY", "super-secret-value")
	t.Setenv("SANDBOXD_DATABASE_URL", "postgres://user:password@host/db")
	t.Setenv("PORT_NUM", "9090")
	_ = Str("ORCHESTRATOR_API_KEY", "")
	_ = Str("SANDBOXD_DATABASE_URL", "")
	_ = Int("PORT_NUM", 8080)

	for _, e := range Resolved() {
		switch e.Key {
		case "ORCHESTRATOR_API_KEY", "SANDBOXD_DATABASE_URL":
			if !e.Secret {
				t.Errorf("%s not marked secret", e.Key)
			}
			if strings.Contains(e.Value, "secret-value") || strings.Contains(e.Value, "password") {
				t.Errorf("%s leaked its value: %q", e.Key, e.Value)
			}
		case "PORT_NUM":
			if e.Value != "9090" {
				t.Errorf("PORT_NUM = %q, want 9090", e.Value)
			}
		}
	}
}

func TestInvalidSecretIsNamedNotPrinted(t *testing.T) {
	Reset()
	t.Setenv("SOME_TOKEN_LIMIT", "not-a-number")
	_ = Int("SOME_TOKEN_LIMIT", 5)
	err := Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "SOME_TOKEN_LIMIT") {
		t.Error("the key must be named so it can be fixed")
	}
	if strings.Contains(err.Error(), "not-a-number") {
		t.Error("a secret's raw value must not appear in the error")
	}
}
