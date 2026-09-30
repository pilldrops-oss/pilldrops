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

func TestTemplateRuntimeAffinity(t *testing.T) {
	// browser ships its tooling only in the baked rootfs; docker runs a
	// daemon that needs a kernel of its own.
	for _, name := range []string{"browser", "docker"} {
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
