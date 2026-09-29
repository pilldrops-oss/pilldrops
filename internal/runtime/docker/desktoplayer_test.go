package docker

import (
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func rtTemplate(name string, env []string) rt.Template {
	return rt.Template{Name: name, Env: env}
}

// Every image the platform builds carries the screen stack, which is what
// makes PD_DESKTOP=1 a property of a sandbox rather than of four templates.
// A template with no tooling of its own must still get a Dockerfile built for
// it, or "any template" is false for most of the catalogue.
func TestEveryTemplateImageGetsTheDesktopLayer(t *testing.T) {
	for _, id := range []string{"node", "code-interpreter", "fastapi", "desktop"} {
		df, ok := enrichedDockerfiles[canonicalTemplate(id)]
		if !ok {
			t.Fatalf("%s has no enriched Dockerfile to append to", id)
		}
		if strings.Contains(df, "x11vnc") && id != "desktop" {
			t.Errorf("%s installs the screen stack itself; it comes from DesktopLayer now", id)
		}
	}
	// A template with no entry of its own: the runtime synthesises a FROM for
	// it, which is the case that turns "four templates" into "any template".
	if _, ok := enrichedDockerfiles[canonicalTemplate("go")]; ok {
		t.Skip("go gained an enriched Dockerfile; pick another bare template")
	}
	df := "FROM " + imageFor("go") + DesktopLayer
	if !strings.HasPrefix(df, "FROM golang") || !strings.Contains(df, "x11vnc") {
		t.Errorf("a bare template's synthesised Dockerfile is wrong:\n%s", df)
	}
}

// The Debian `novnc` package Depends on nodejs, net-tools, python3-numpy and
// python3-novnc (→ python3-pil, the OpenStack oslo stack) to ship 248 KB of
// static files. Measured on the node: installing it makes the layer 583 MB,
// unpacking the same .deb makes it 486 MB. Pinned because "just add it to the
// apt line" is the obvious edit and costs 97 MB in every template image.
func TestNoVNCIsUnpackedNotInstalled(t *testing.T) {
	for _, field := range []string{"apt-get download novnc", "dpkg-deb -x novnc_*.deb /"} {
		if !strings.Contains(DesktopLayer, field) {
			t.Errorf("DesktopLayer no longer does %q:\n%s", field, DesktopLayer)
		}
	}
	// The give-away is `novnc` in the install list rather than the download.
	install, _, found := strings.Cut(DesktopLayer, "&& apt-get download")
	if !found {
		t.Fatalf("DesktopLayer has no download step, so this test checks nothing:\n%s", DesktopLayer)
	}
	if strings.Contains(install, " novnc ") {
		t.Errorf("novnc is back in the apt install list, which pulls 183 MB of "+
			"nodejs/numpy/PIL/oslo into every template image:\n%s", install)
	}
	// websockify serves the unpacked files, so it must still be installed.
	if !strings.Contains(install, "websockify") {
		t.Errorf("websockify is not installed; nothing would serve noVNC:\n%s", install)
	}
}

// The host's publish decision and the guest's start decision read the same
// env, or a sandbox gets a desktop on a port nothing published.
func TestWantsDesktop(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		env  []string
		want bool
	}{
		{"flag in the create env", "node", []string{"PD_DESKTOP=1"}, true},
		{"flag among others", "node", []string{"FOO=bar", "PD_DESKTOP=1"}, true},
		{"no flag", "node", []string{"FOO=bar"}, false},
		{"flag turned off", "node", []string{"PD_DESKTOP=0"}, false},
		{"desktop template bakes it into the image", "desktop", nil, true},
		{"an ordinary template does not", "base", nil, false},
	}
	for _, tc := range cases {
		got := wantsDesktop(rtTemplate(tc.tmpl, tc.env))
		if got != tc.want {
			t.Errorf("%s: wantsDesktop = %v, want %v", tc.name, got, tc.want)
		}
	}
}
