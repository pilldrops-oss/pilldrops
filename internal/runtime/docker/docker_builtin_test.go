package docker

import (
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestResolveBuiltinAndroid(t *testing.T) {
	for _, name := range []string{"android", "android-emulator", "Android-14"} {
		spec, devices := resolveBuiltin(rt.BootSpec{Template: rt.Template{Name: name}})
		if spec.Template.Image != "budtmo/docker-android:emulator_14.0" {
			t.Fatalf("%s: image = %q", name, spec.Template.Image)
		}
		if len(spec.Template.Ports) == 0 || spec.Template.Ports[0] != 6080 {
			t.Fatalf("%s: primary port = %v, want 6080 first (noVNC screen)", name, spec.Template.Ports)
		}
		if len(devices) != 1 || devices[0] != "/dev/kvm" {
			t.Fatalf("%s: devices = %v, want [/dev/kvm]", name, devices)
		}
		if len(spec.Template.Env) == 0 {
			t.Fatalf("%s: expected emulator env", name)
		}
	}
}

func TestResolveBuiltinPassthrough(t *testing.T) {
	explicit := rt.BootSpec{Template: rt.Template{Name: "android", Image: "custom:1"}}
	if spec, devices := resolveBuiltin(explicit); spec.Template.Image != "custom:1" || devices != nil {
		t.Fatalf("explicit image was rewritten: %+v %v", spec.Template, devices)
	}
	plain := rt.BootSpec{Template: rt.Template{Name: "code-interpreter"}}
	if spec, devices := resolveBuiltin(plain); spec.Template.Image != "" || devices != nil {
		t.Fatalf("non-service template was rewritten: %+v %v", spec.Template, devices)
	}
}

// mobile-test is headless android plus the injected harness, session server as primary port.
func TestResolveBuiltinMobileTest(t *testing.T) {
	for _, name := range []string{"mobile-test", "android-test", "mobile-e2e"} {
		spec, devices := resolveBuiltin(rt.BootSpec{Template: rt.Template{Name: name}})
		if spec.Template.Image != "budtmo/docker-android:emulator_14.0" {
			t.Fatalf("%s: image = %q", name, spec.Template.Image)
		}
		if len(spec.Template.Ports) == 0 || spec.Template.Ports[0] != sessionPort {
			t.Fatalf("%s: primary port = %v, want %d (session server)", name, spec.Template.Ports, sessionPort)
		}
		for _, e := range spec.Template.Env {
			if e == "WEB_VNC=true" {
				t.Fatalf("%s: session template must not enable the VNC stack", name)
			}
		}
		if len(devices) != 1 || devices[0] != "/dev/kvm" {
			t.Fatalf("%s: devices = %v", name, devices)
		}
		if bs := builtinServices[canonicalTemplate(name)]; !bs.harness {
			t.Fatalf("%s: harness not enabled", name)
		}
	}
}

func TestSessionHarnessEmbedded(t *testing.T) {
	for _, f := range []string{"session_harness/harness.sh", "session_harness/index.html"} {
		data, err := sessionHarness.ReadFile(f)
		if err != nil || len(data) == 0 {
			t.Fatalf("embedded %s: err=%v len=%d", f, err, len(data))
		}
	}
}

func TestResolveBuiltinAndroidHeadless(t *testing.T) {
	spec, devices := resolveBuiltin(rt.BootSpec{Template: rt.Template{Name: "android-headless"}})
	if spec.Template.Image != "budtmo/docker-android:emulator_14.0" {
		t.Fatalf("image = %q", spec.Template.Image)
	}
	if len(spec.Template.Ports) == 0 || spec.Template.Ports[0] != 4723 {
		t.Fatalf("primary port = %v, want 4723 (Appium)", spec.Template.Ports)
	}
	for _, e := range spec.Template.Env {
		if e == "WEB_VNC=true" {
			t.Fatal("headless variant must not enable the VNC stack")
		}
	}
	if len(devices) != 1 || devices[0] != "/dev/kvm" {
		t.Fatalf("devices = %v", devices)
	}
}

// TestResolveBuiltin_DeviceBoundary checks /dev/kvm reaches only the builtin
// android templates and never a tenant-supplied image.
func TestResolveBuiltin_DeviceBoundary(t *testing.T) {
	for _, name := range []string{"android", "android-headless", "mobile-test"} {
		_, devices := resolveBuiltin(rt.BootSpec{Template: rt.Template{Name: name}})
		if len(devices) != 1 || devices[0] != "/dev/kvm" {
			t.Fatalf("%s: devices = %v, want [/dev/kvm]", name, devices)
		}
	}
	if _, devices := resolveBuiltin(rt.BootSpec{Template: rt.Template{Name: "python-3.11"}}); devices != nil {
		t.Fatalf("python-3.11: devices = %v, want nil (non-builtin template must not get a device)", devices)
	}
	// A tenant-supplied Image must yield no devices even when Name canonicalizes to a builtin.
	spec := rt.BootSpec{Template: rt.Template{Name: "android", Image: "docker.io/evil/android:latest"}}
	if _, devices := resolveBuiltin(spec); devices != nil {
		t.Fatalf("image-backed android request: devices = %v, want nil (image-backed request must never receive a host device)", devices)
	}
}

// TestBuiltinServices_DeviceInUnconditionalArgv source-inspects bootImage to
// check the --device loop and the hardening flags share the unconditional section.
func TestBuiltinServices_DeviceInUnconditionalArgv(t *testing.T) {
	body := sourceContaining(t, `func (r *Runtime) bootImage(`)

	fnStart := strings.Index(body, "func (r *Runtime) bootImage(")
	if fnStart < 0 {
		t.Fatal("bootImage has moved or been renamed; this test no longer checks anything")
	}
	// The last unconditional line before bootImage branches on service/agent.
	agentTokenIdx := strings.Index(body[fnStart:], "var agentToken, agentHash string")
	if agentTokenIdx < 0 {
		t.Fatal("the agentToken anchor has moved; this test no longer checks anything")
	}
	unconditional := body[fnStart : fnStart+agentTokenIdx]

	for _, want := range []string{
		`"--device", d`,
		`"--security-opt", "no-new-privileges"`,
		`"--pids-limit"`,
		`nofile=%d:%d`,
	} {
		if !strings.Contains(unconditional, want) {
			t.Errorf("bootImage's unconditional argv section does not contain %q — the --device loop and H6 hardening (pids-limit/ulimit/no-new-privileges) must stay in the same unconditional section so builtin service templates (android/android-headless/mobile-test) keep getting both together", want)
		}
	}
}
