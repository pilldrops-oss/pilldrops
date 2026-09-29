package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func TestPreviewTargetServesTerminalOnItsOwnPort(t *testing.T) {
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "code-interpreter"})

	target, err := d.PreviewTarget(ctx, sb.ID, rt.TerminalPort, "default")
	if err != nil {
		t.Fatalf("PreviewTarget(%d): %v", rt.TerminalPort, err)
	}
	if target != "127.0.0.1:7681" {
		t.Fatalf("terminal port routed to %q, want 127.0.0.1:7681", target)
	}
}

func TestPreviewTargetDoesNotServeTerminalOnAppPorts(t *testing.T) {
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "code-interpreter"})

	for _, port := range []int{8000, 3000} {
		target, err := d.PreviewTarget(ctx, sb.ID, port, "default")
		if target == "127.0.0.1:7681" {
			t.Errorf("port %d resolved to the TERMINAL (%q) — preview and terminal are the same thing again", port, target)
		}
		// The fake answers any port, so this only asserts the terminal is not substituted.
		if err != nil && !errors.Is(err, rt.ErrNoAppOnPort) {
			t.Errorf("port %d: unexpected error %v", port, err)
		}
	}
}

// Guards the platform-port exemption in the single-port check.
func TestPreviewTargetServesTerminalEvenWithAnAppRunning(t *testing.T) {
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	target, err := d.PreviewTarget(ctx, sb.ID, rt.TerminalPort, "default")
	if err != nil {
		t.Fatalf("PreviewTarget(terminal) with an app running: %v", err)
	}
	if target != "127.0.0.1:7681" {
		t.Fatalf("terminal routed to %q, want 127.0.0.1:7681", target)
	}
}

func TestPreviewTargetPrefersAppOverTerminal(t *testing.T) {
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	target, err := d.PreviewTarget(ctx, sb.ID, 8000, "default")
	if err != nil {
		t.Fatalf("PreviewTarget: %v", err)
	}
	if target != "127.0.0.1:8000" {
		t.Fatalf("got target %q, want the app port 127.0.0.1:8000", target)
	}
}
