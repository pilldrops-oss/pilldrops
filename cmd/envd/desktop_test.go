package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The flag is the whole gate: a sandbox without PD_DESKTOP=1 must start no
// screen, which is what keeps the memory cost at zero.
func TestDesktopFlagGatesTheStack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	templateEnvDir = dir
	t.Cleanup(func() { templateEnvDir = "/etc" })

	if got := envLookup(buildSessionEnv(nil), "PD_DESKTOP"); got != "" {
		t.Fatalf("PD_DESKTOP = %q with nothing setting it, want empty", got)
	}
	// Firecracker's delivery: the create env lands in template.env after boot.
	if err := os.WriteFile(filepath.Join(dir, "template.env"), []byte("PD_DESKTOP=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := envLookup(buildSessionEnv(nil), "PD_DESKTOP"); got != "1" {
		t.Errorf("PD_DESKTOP = %q after the create env was pushed, want 1", got)
	}
	// Docker's delivery: the same key in envd's own process env.
	os.Remove(filepath.Join(dir, "template.env"))
	t.Setenv("PD_DESKTOP", "1")
	if got := envLookup(buildSessionEnv(nil), "PD_DESKTOP"); got != "1" {
		t.Errorf("PD_DESKTOP = %q from the process env, want 1", got)
	}
}

// DISPLAY must appear in sessions only once a display exists: exporting it
// with no X server turns every GUI command into "cannot open display".
func TestDisplayOnlyOnceTheDesktopIsUp(t *testing.T) {
	dir := t.TempDir()
	desktopUp.Store(false)
	t.Cleanup(func() { desktopUp.Store(false) })

	if got := envLookup(buildSessionEnvFrom(dir, nil), "DISPLAY"); got != "" {
		t.Errorf("DISPLAY = %q with no desktop running, want empty", got)
	}
	desktopUp.Store(true)
	if got := envLookup(buildSessionEnvFrom(dir, nil), "DISPLAY"); got != desktopDisplay {
		t.Errorf("DISPLAY = %q with the desktop up, want %q", got, desktopDisplay)
	}
	// A caller naming its own display still wins.
	if got := envLookup(buildSessionEnvFrom(dir, map[string]string{"DISPLAY": ":9"}), "DISPLAY"); got != ":9" {
		t.Errorf("DISPLAY = %q, want the request's :9", got)
	}
}

// waitForX is what keeps fluxbox and x11vnc from racing Xvfb's socket.
func TestWaitForX(t *testing.T) {
	dir := t.TempDir()
	xSocketDir = dir
	t.Cleanup(func() { xSocketDir = "/tmp/.X11-unix" })

	if waitForX(150 * time.Millisecond) {
		t.Error("waitForX returned true with no X socket present")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, "X0"), nil, 0600)
	}()
	if !waitForX(3 * time.Second) {
		t.Error("waitForX did not see the socket that appeared while it waited")
	}
}

func TestFluxboxMenuFromDesktopEntries(t *testing.T) {
	apps := t.TempDir()
	home := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(apps, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("chromium.desktop", "[Desktop Entry]\nName=Chromium\nExec=chromium %U\nType=Application\n")
	write("hidden.desktop", "[Desktop Entry]\nName=Installer\nExec=install-me\nNoDisplay=true\n")
	write("cli.desktop", "[Desktop Entry]\nName=htop\nExec=htop\nTerminal=true\n")
	write("noexec.desktop", "[Desktop Entry]\nName=Broken\n")
	// A later group's Name is an action's, not the application's.
	write("actions.desktop", "[Desktop Entry]\nName=Files\nExec=nautilus\n[Desktop Action New]\nName=New Window\nExec=nautilus --new\n")

	if err := writeFluxboxMenu(home, apps); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".fluxbox", "menu"))
	if err != nil {
		t.Fatal(err)
	}
	menu := string(b)

	for _, want := range []string{"[exec] (Chromium) {chromium}", "[exec] (Files) {nautilus}", "[exec] (Terminal) {xterm}"} {
		if !strings.Contains(menu, want) {
			t.Errorf("menu is missing %q:\n%s", want, menu)
		}
	}
	for _, unwanted := range []string{"Installer", "htop", "Broken", "New Window"} {
		if strings.Contains(menu, unwanted) {
			t.Errorf("menu contains %q, which should have been skipped:\n%s", unwanted, menu)
		}
	}
	// %U would be passed to the program as a literal argument.
	if strings.Contains(menu, "%U") {
		t.Errorf("field code left in the menu:\n%s", menu)
	}
	if !strings.Contains(menu, "[begin] (Sandbox)") || !strings.Contains(menu, "[end]") {
		t.Errorf("menu is not a complete fluxbox menu:\n%s", menu)
	}
	// fluxbox reads entries only between [begin] and [end].
	if strings.Index(menu, "[begin]") > strings.Index(menu, "[exec]") ||
		strings.LastIndex(menu, "[exec]") > strings.Index(menu, "[end]") {
		t.Errorf("entries fall outside the [begin]/[end] block:\n%s", menu)
	}
}

// An image with no .desktop files at all must still get a usable menu, or the
// desktop opens with no way to start anything.
func TestFluxboxMenuWithNoApplications(t *testing.T) {
	home := t.TempDir()
	if err := writeFluxboxMenu(home, filepath.Join(t.TempDir(), "nothing-here")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".fluxbox", "menu"))
	if !strings.Contains(string(b), "[exec] (Terminal) {xterm}") {
		t.Errorf("empty applications dir left no terminal entry:\n%s", b)
	}
}

// "%%" is a literal percent, not a field code. Debian's own x11vnc.desktop ships
// `Exec=x11vnc ... -o %%HOME/.x11vnc.log.%%VNCDISPLAY`; x11vnc expands %HOME
// itself, so writing "%%HOME" through hands it a path it does not know.
// Observed verbatim in a live sandbox's menu on 2026-09-21.
func TestExecCodesUnescapeALiteralPercent(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"x11vnc -o %%HOME/.x11vnc.log.%%VNCDISPLAY", "x11vnc -o %HOME/.x11vnc.log.%VNCDISPLAY"},
		{"chromium %U", "chromium "},       // field code drops
		{"printf %%f", "printf %f"},        // escaped, so NOT a field code
		{"fmt %s and %q", "fmt %s and %q"}, // unknown codes pass through
		{"trailing %", "trailing %"},       // a lone % at the end
	} {
		if got := expandExecCodes(c.in); got != c.want {
			t.Errorf("expandExecCodes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
