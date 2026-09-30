package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestPlatformPortsAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for name, p := range map[string]int{"TerminalPort": TerminalPort, "AgentPort": AgentPort, "DesktopPort": DesktopPort} {
		if other, dup := seen[p]; dup {
			t.Fatalf("%s and %s are both %d; one service would shadow the other", name, other, p)
		}
		seen[p] = name
	}
}

func TestIsPlatformPort(t *testing.T) {
	for _, p := range []int{TerminalPort, AgentPort, DesktopPort} {
		if !IsPlatformPort(p) {
			t.Errorf("IsPlatformPort(%d) = false for a platform port", p)
		}
	}
	// A false positive here previews a terminal instead of the customer's app.
	for _, p := range []int{80, 443, 3000, 5000, 5173, 8000, 8080, 8888, 9000} {
		if IsPlatformPort(p) {
			t.Errorf("IsPlatformPort(%d) = true; a user app on this port would be "+
				"shadowed by the platform", p)
		}
	}
	for _, p := range []int{0, -1, TerminalPort - 1, TerminalPort + 1, AgentPort - 1, AgentPort + 1,
		DesktopPort - 1, DesktopPort + 1} {
		if IsPlatformPort(p) {
			t.Errorf("IsPlatformPort(%d) = true, want false", p)
		}
	}
	// envd's own port is never previewed, so it is deliberately not a platform port.
	if IsPlatformPort(6001) {
		t.Error("IsPlatformPort(6001) = true; envd's port is not a previewable platform port")
	}
}

func TestSnapshotRefIsZero(t *testing.T) {
	if !(SnapshotRef{}).IsZero() {
		t.Error("a zero SnapshotRef must report IsZero")
	}
	if (SnapshotRef{Path: "/snap/mem"}).IsZero() {
		t.Error("a SnapshotRef with a path is not zero")
	}
}

// The API maps each sentinel to a different status code.
func TestSentinelErrorsAreDistinct(t *testing.T) {
	sentinels := map[string]error{
		"ErrNotFound":    ErrNotFound,
		"ErrPaused":      ErrPaused,
		"ErrNoAppOnPort": ErrNoAppOnPort,
		"ErrUnsupported": ErrUnsupported,
	}
	for aName, a := range sentinels {
		for bName, b := range sentinels {
			if aName == bName {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("errors.Is(%s, %s) is true; these map to different status codes", aName, bName)
			}
		}
		if a.Error() == "" {
			t.Errorf("%s has an empty message", aName)
		}
	}
}

func TestAppLogPathIsRelativeToTheSandboxRoot(t *testing.T) {
	if strings.HasPrefix(AppLogPath, "/") {
		t.Errorf("AppLogPath = %q; an absolute path escapes the sandbox root and "+
			"will not travel with a snapshot", AppLogPath)
	}
	if strings.Contains(AppLogPath, "..") {
		t.Errorf("AppLogPath = %q contains a parent traversal", AppLogPath)
	}
}
