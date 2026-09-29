package orchestrator

import "testing"

func TestWantRuntime(t *testing.T) {
	cases := []struct{ runtime, isolation, want string }{
		{"firecracker", "", "firecracker"},
		{"Docker", "", "docker"},
		{"", "vm", "firecracker"},
		{"", "microvm", "firecracker"},
		{"", "hardware", "firecracker"},
		{"", "container", "docker"},
		{"", "", ""},
		{"bogus", "", ""},
		{"", "bogus", ""},
		{"firecracker", "container", "firecracker"}, // explicit runtime wins over alias
	}
	for _, c := range cases {
		if got := wantRuntime(c.runtime, c.isolation); got != c.want {
			t.Errorf("wantRuntime(%q,%q) = %q, want %q", c.runtime, c.isolation, got, c.want)
		}
	}
}

// Aliases inherit the pin through the catalog's Language field.
func TestTemplateRuntimeAffinity(t *testing.T) {
	for _, name := range []string{
		"android", "android-emulator", "android-14",
		"android-headless", "android-ci", "android-appium",
		"mobile-test", "android-test", "mobile-e2e",
	} {
		if got := templateRuntime(name); got != "docker" {
			t.Errorf("templateRuntime(%q) = %q, want docker", name, got)
		}
	}
	// browser, security and loadtest ship their tooling only in the baked
	// rootfs; docker runs a daemon that needs a kernel of its own.
	for _, name := range []string{"browser", "security", "loadtest", "scan", "k6", "docker"} {
		if got := templateRuntime(name); got != "firecracker" {
			t.Errorf("templateRuntime(%q) = %q, want firecracker", name, got)
		}
	}
	for _, name := range []string{"", "base", "code-interpreter", "playwright", "no-such-template"} {
		if got := templateRuntime(name); got != "" {
			t.Errorf("templateRuntime(%q) = %q, want \"\" (any runtime)", name, got)
		}
	}
}
