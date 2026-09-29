// On-demand screen. A sandbox created with PD_DESKTOP=1 gets a virtual
// display, a window manager and a VNC-over-WebSocket viewer on a platform
// port; a sandbox without the flag starts none of it and pays no memory for a
// screen nobody is looking at. Same rule as startTerminalServer: a missing
// binary or a failed bind logs one line and is never fatal — a sandbox that
// could not open a display is still a working sandbox.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// desktopPort must match runtime.DesktopPort, which is what the preview
	// proxy dials and what IsPlatformPort exempts from the
	// single-published-port rule. Fixed, not configurable: the platform's own
	// reserved-name list refuses PD_DESKTOP_PORT by name (see
	// internal/sandbox/envname.go), because a caller moving the viewer would
	// only break its own preview.
	desktopPort = 6080
	// desktopDisplay is the X display the stack owns; exported into every
	// session so a GUI program started by exec lands on the screen being watched.
	desktopDisplay = ":0"
	// /tmp, not /var/log: envd runs as an unprivileged user on the Docker
	// runtime (measured: uid 1001) and cannot create a file under /var/log, so
	// the log silently went nowhere.
	desktopLogPath = "/tmp/desktop.log"
	// vncPort is loopback-only: websockify is the only thing that dials it.
	vncPort = 5900
)

// desktopUp is read by buildSessionEnv, so DISPLAY appears in exec and
// terminal sessions only once a display actually exists.
var desktopUp atomic.Bool

// desktopOnce keeps a second PD_DESKTOP=1 (a /config refresh, a fork
// re-sending the parent's env) from starting a second X server on :0.
var desktopOnce sync.Once

// maybeStartDesktop starts the screen stack if this sandbox's environment asks
// for one. Called at boot and again after SetEnv, because the two runtimes
// deliver the create env differently: Docker puts it in envd's own process env
// (`-e`), Firecracker pushes it over the control channel after boot, so the
// boot-time read sees nothing and the SetEnv-time read is the one that fires.
func maybeStartDesktop() {
	if envLookup(buildSessionEnv(nil), "PD_DESKTOP") != "1" {
		return
	}
	desktopOnce.Do(func() { go startDesktop() })
}

// envLookup reads one key out of a K=V environment slice.
func envLookup(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// startDesktop brings up Xvfb, fluxbox, x11vnc and websockify, in that order,
// logging each child to desktopLogPath. It returns once the viewer is
// serving, or earlier with one log line if the image does not hold the stack.
func startDesktop() {
	need := []string{"Xvfb", "fluxbox", "x11vnc", "websockify"}
	var missing []string
	for _, bin := range need {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "envd: PD_DESKTOP=1 but this image has no screen stack (missing %s); "+
			"no desktop on this sandbox\n", strings.Join(missing, " "))
		return
	}

	// Built once for the whole stack rather than per child: every child gets
	// the same environment, and startDesktop runs once at boot.
	env := append(buildSessionEnv(nil), "DISPLAY="+desktopDisplay)

	log, err := os.OpenFile(desktopLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// A read-only /var must not cost the sandbox its screen.
		log = nil
	}

	// +extension RANDR is what lets the viewer resize the display to its own
	// window instead of scaling a fixed framebuffer; x11vnc -xrandr is the
	// other half. 1920x1080 is the starting size, not a ceiling.
	desktopRun(env, log, "Xvfb", desktopDisplay, "-screen", "0", "1920x1080x24", "+extension", "RANDR", "-ac")
	if !waitForX(10 * time.Second) {
		fmt.Fprintf(os.Stderr, "envd: Xvfb did not create %s; no desktop on this sandbox\n", xSocketPath())
		return
	}

	// The menu is the launcher: without it a right-click on the root window
	// offers fluxbox's compiled-in defaults, which name programs this image
	// does not have.
	if err := writeFluxboxMenu(envLookup(env, "HOME"), "/usr/share/applications"); err != nil {
		fmt.Fprintf(os.Stderr, "envd: fluxbox menu: %v\n", err)
	}

	desktopRun(env, log, "fluxbox")
	// TWO instances, not one. autocutsel bridges a single selection to the
	// cutbuffer, so `autocutsel -fork` alone syncs CLIPBOARD and leaves
	// PRIMARY untouched — measured: with one instance, setting CLIPBOARD and
	// reading PRIMARY gives "target STRING not available", and with both it
	// round-trips in both directions. The cutbuffer is the bridge between
	// them, which is why each selection needs its own process.
	// Optional: an image without autocutsel still shows a desktop.
	if _, err := exec.LookPath("autocutsel"); err == nil {
		desktopRun(env, log, "autocutsel", "-fork")
		desktopRun(env, log, "autocutsel", "-selection", "PRIMARY", "-fork")
	}
	// -shared: every connected viewer has input. There is deliberately no
	// takeover; see docs/template-registry.md §Desktop.
	// ponytail: shared control, add a websockify wrapper that closes the
	// previous client when someone needs exclusive input.
	desktopRun(env, log, "x11vnc", "-display", desktopDisplay, "-forever", "-shared", "-nopw",
		"-rfbport", strconv.Itoa(vncPort), "-xrandr", "-quiet", "-bg")

	// websockify binds every interface because the preview proxy dials the
	// sandbox from outside it; the VNC port behind it stays on loopback. This
	// is the exposure every other app port in a sandbox already has — the
	// per-sandbox namespace (or the shared bridge's port isolation) is what
	// keeps a peer out, and the preview surface in front is token-gated.
	desktopRun(env, log, "websockify", "--web=/usr/share/novnc",
		strconv.Itoa(desktopPort), fmt.Sprintf("localhost:%d", vncPort))

	desktopUp.Store(true)
	fmt.Printf("envd: desktop listening on 0.0.0.0:%d (display %s)\n", desktopPort, desktopDisplay)
}

// desktopRun starts one child of the stack with its output in the desktop log
// and reaps it when it dies, so a crashed component does not become a zombie.
func desktopRun(env []string, log *os.File, name string, args ...string) {
	// context.Background deliberately: these children are the sandbox's screen
	// and outlive whatever call asked for a desktop. Nothing may cancel them
	// but their own exit.
	cmd := exec.CommandContext(context.Background(), name, args...)
	// The session environment, not envd's own. envd's is a bare init
	// environment and every assumption about it has been wrong once already:
	// no DISPLAY, so fluxbox and autocutsel died instantly while the three
	// children that take the display as an argument came up (a screen with no
	// window manager and no clipboard); and HOME=/ on Firecracker, so fluxbox
	// kept its state and read its menu from the filesystem root while every
	// shell in the sandbox had HOME=/root. Using what exec and the terminal
	// get means the desktop's children and the user's shells agree about HOME,
	// PATH and anything the template baked — the whole class of bug, not the
	// two instances of it that were found.
	cmd.Env = env
	if log != nil {
		cmd.Stdout, cmd.Stderr = log, log
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "envd: desktop: start %s: %v\n", name, err)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			fmt.Fprintf(os.Stderr, "envd: desktop: %s exited: %v\n", name, err)
		}
	}()
}

// xSocketDir is a var so tests can redirect the readiness probe.
var xSocketDir = "/tmp/.X11-unix"

func xSocketPath() string { return filepath.Join(xSocketDir, "X0") }

// waitForX blocks until the X server's socket appears. Xvfb answers no
// connection before it exists, so fluxbox and x11vnc would both die on a race.
func waitForX(limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(xSocketPath()); err == nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// writeFluxboxMenu renders a root menu from the image's own .desktop entries,
// so what the menu offers is what the sandbox actually holds. xterm and the
// two fixed entries are appended unconditionally: a shell and a way out are
// what make the desktop usable when the image ships no .desktop files at all.
func writeFluxboxMenu(home, appsDir string) error {
	if home == "" {
		home = "/root"
	}
	entries := desktopEntries(appsDir)
	entries = append(entries, menuEntry{"Terminal", "xterm"})

	var b strings.Builder
	b.WriteString("# generated by envd at desktop start from " + appsDir + "\n")
	b.WriteString("[begin] (Sandbox)\n")
	for _, e := range entries {
		b.WriteString("[exec] (" + e.Name + ") {" + e.Exec + "}\n")
	}
	b.WriteString("[workspaces] (Workspaces)\n")
	b.WriteString("[restart] (Restart)\n")
	b.WriteString("[end]\n")

	dir := filepath.Join(home, ".fluxbox")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "menu"), []byte(b.String()), 0644)
}

type menuEntry struct{ Name, Exec string }

// expandExecCodes resolves an Exec line's % sequences in ONE pass, left to
// right, which is what the freedesktop spec requires and what a
// replace-each-code loop cannot do: "%%" is a literal percent, so "%%f" is the
// two characters "%f" and not a field code, and "%%HOME" is "%HOME". Measured
// on a live sandbox: Debian's x11vnc.desktop ships
// `-o %%HOME/.x11vnc.log.%%VNCDISPLAY`, and the loop wrote both "%%" through
// untouched. Field codes (%f, %U, …) pass a file manager's selection to the
// program; a root-menu entry has no selection, so they drop.
func expandExecCodes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch c := s[i+1]; c {
		case '%':
			b.WriteByte('%')
		case 'f', 'F', 'u', 'U', 'i', 'c', 'k', 'd', 'D', 'n', 'N', 'v', 'm':
			// dropped
		default:
			b.WriteByte('%')
			b.WriteByte(c)
		}
		i++
	}
	return b.String()
}

// desktopEntries reads Name and Exec out of the .desktop files in dir. A file
// that declares NoDisplay or Terminal is skipped — running it from a window
// manager menu with no terminal attached shows the user nothing.
func desktopEntries(dir string) []menuEntry {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.desktop")) // Glob sorts
	var out []menuEntry
	for _, p := range paths {
		if e, ok := parseDesktopEntry(p); ok {
			out = append(out, e)
		}
	}
	return out
}

// parseDesktopEntry reads one freedesktop .desktop file, taking only the keys
// in its [Desktop Entry] group; a later group's Name would be an action's.
func parseDesktopEntry(path string) (menuEntry, bool) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return menuEntry{}, false
	}
	defer func() { _ = f.Close() }()

	var e menuEntry
	inEntry := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inEntry = line == "[Desktop Entry]"
			continue
		}
		if !inEntry {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Name":
			if e.Name == "" { // the untranslated key; Name[de] is not it
				e.Name = strings.TrimSpace(v)
			}
		case "Exec":
			e.Exec = strings.TrimSpace(expandExecCodes(strings.TrimSpace(v)))
		case "NoDisplay", "Terminal":
			if strings.EqualFold(strings.TrimSpace(v), "true") {
				return menuEntry{}, false
			}
		}
	}
	// Braces would end the {command} block early and corrupt the whole menu.
	if e.Name == "" || e.Exec == "" || strings.ContainsAny(e.Name+e.Exec, "(){}\n") {
		return menuEntry{}, false
	}
	return e, true
}
