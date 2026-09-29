package nodeboot

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTemplateList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"base"}},
		{"base", []string{"base"}},
		{"base,node, code-interpreter ,", []string{"base", "node", "code-interpreter"}},
		{" , ", []string{"base"}},
	}
	for _, tc := range cases {
		if got := TemplateList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("TemplateList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEnvDefaults(t *testing.T) {
	t.Setenv("NODEBOOT_TEST_SET", "x")
	if got := Env("NODEBOOT_TEST_SET", "d"); got != "x" {
		t.Errorf("Env set = %q, want x", got)
	}
	if got := Env("NODEBOOT_TEST_UNSET_ZZZ", "d"); got != "d" {
		t.Errorf("Env unset = %q, want d", got)
	}
	t.Setenv("NODEBOOT_TEST_EMPTY", "")
	if got := Env("NODEBOOT_TEST_EMPTY", "d"); got != "d" {
		t.Errorf("Env set-but-empty = %q, want d (empty means unset)", got)
	}
	t.Setenv("NODEBOOT_TEST_INT", "7")
	if got := EnvInt("NODEBOOT_TEST_INT", 3); got != 7 {
		t.Errorf("EnvInt set = %d, want 7", got)
	}
	t.Setenv("NODEBOOT_TEST_INT", "notanint")
	if got := EnvInt("NODEBOOT_TEST_INT", 3); got != 3 {
		t.Errorf("EnvInt bad = %d, want 3", got)
	}
}

func TestLocalURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":          "http://localhost:8080",
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"[::1]:8080":     "http://[::1]:8080",
	} {
		if got := LocalURL(addr); got != want {
			t.Errorf("LocalURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestLocalPreviewSecretIsMintedOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	first := LocalPreviewSecret(dir)
	if len(first) != 64 {
		t.Fatalf("secret %q, want 64 hex chars", first)
	}
	if again := LocalPreviewSecret(dir); again != first {
		t.Fatal("a restart minted a new secret, which invalidates every preview link already handed out")
	}
	fi, err := os.Stat(filepath.Join(dir, "preview.secret"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("preview.secret mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestHeartbeatIntervalIsJitteredAroundFiveSeconds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := heartbeatInterval()
		if d < 4*time.Second || d >= 6*time.Second {
			t.Fatalf("interval %v outside [4s, 6s)", d)
		}
		seen[d.Truncate(100*time.Millisecond)] = true
	}
	if len(seen) < 5 {
		t.Fatal("the interval barely varies, so nodes that start together stay in lockstep")
	}
}
