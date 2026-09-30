package daemon

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// setFastWakeTimeout shrinks previewWakeReadyTimeout; the Fake's address never listens.
func setFastWakeTimeout(t *testing.T) {
	t.Helper()
	orig := previewWakeReadyTimeout
	previewWakeReadyTimeout = 20 * time.Millisecond
	t.Cleanup(func() { previewWakeReadyTimeout = orig })
}

// $PORT must be the declared port, not the template default; a caller-provided PORT wins.
func TestStartServerInjectsDeclaredPort(t *testing.T) {
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "node app.js", 3000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if got := fake.LastServerEnv(sb.ID)["PORT"]; got != "3000" {
		t.Fatalf("PORT env = %q, want 3000 (declared port)", got)
	}

	if err := d.StartServer(ctx, sb.ID, "node app.js", 3000, map[string]string{"PORT": "5555"}); err != nil {
		t.Fatalf("start server (explicit PORT): %v", err)
	}
	if got := fake.LastServerEnv(sb.ID)["PORT"]; got != "5555" {
		t.Fatalf("PORT env = %q, want caller's 5555", got)
	}

	if err := d.StartServer(ctx, sb.ID, "node app.js", 0, nil); err != nil {
		t.Fatalf("start server (no port): %v", err)
	}
	if env := fake.LastServerEnv(sb.ID); env["PORT"] != "" {
		t.Fatalf("PORT env = %q, want unset when no port declared", env["PORT"])
	}
}

func TestPreviewTargetWakesPausedSandbox(t *testing.T) {
	setFastWakeTimeout(t)
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State != StatePaused {
		t.Fatalf("expected paused, got %s", got.State)
	}

	target, err := d.PreviewTarget(ctx, sb.ID, 8000, "default")
	if err != nil {
		t.Fatalf("preview target: %v", err)
	}
	if target == "" {
		t.Fatalf("empty target")
	}
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("preview did not wake sandbox; state = %s", got.State)
	}
}

func TestPreviewTargetUnknown(t *testing.T) {
	d := New(Config{Runtime: runtimetest.NewFake()})
	if _, err := d.PreviewTarget(context.Background(), "nope", 8000, "default"); err == nil {
		t.Fatalf("expected error for unknown sandbox")
	}
}

func TestPreviewTargetRejectsUnpublishedPort(t *testing.T) {
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	if _, err := d.PreviewTarget(ctx, sb.ID, 8000, "default"); err != nil {
		t.Fatalf("published port 8000 should route: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 9999, "default"); !errors.Is(err, rt.ErrNoAppOnPort) {
		t.Fatalf("unpublished port 9999 err = %v, want ErrNoAppOnPort", err)
	}
}

func TestPreviewWakeRestartsApp(t *testing.T) {
	setFastWakeTimeout(t)
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 8000, "default"); err != nil {
		t.Fatalf("preview target: %v", err)
	}
	if got := fake.ServerLog(sb.ID); len(got) != 1 || got[0] != "python3 app.py" {
		t.Fatalf("app was not restarted on wake; ServerLog = %v", got)
	}
}

func TestPreviewTargetUnrestrictedWhenNoPublishedPort(t *testing.T) {
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if _, err := d.PreviewTarget(ctx, sb.ID, 3000, "default"); err != nil {
		t.Fatalf("unrestricted sandbox should route any port: %v", err)
	}
}

func TestWaitDialable_RealListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	start := time.Now()
	waitDialable(context.Background(), addr, 3*time.Second)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("waitDialable took %v against a live listener; expected < 500ms", elapsed)
	}
}

func TestWaitDialable_ClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	const shortTimeout = 80 * time.Millisecond
	start := time.Now()
	waitDialable(context.Background(), addr, shortTimeout)
	elapsed := time.Since(start)

	if elapsed > shortTimeout+200*time.Millisecond {
		t.Errorf("waitDialable ran for %v; expected <= %v", elapsed, shortTimeout+200*time.Millisecond)
	}
}

// The Fake's address never listens, so this exercises the readiness-poll timeout path.
func TestPreviewWakeWaitsForReadiness(t *testing.T) {
	setFastWakeTimeout(t)
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	start := time.Now()
	target, err := d.PreviewTarget(ctx, sb.ID, 8000, "default")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("PreviewTarget: %v", err)
	}
	if target == "" {
		t.Fatal("PreviewTarget returned empty target")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("PreviewTarget took %v; expected < 500ms with fast wake timeout", elapsed)
	}
}

// On a multi-port runtime any port routes to the guest once a primary app is registered.
func TestPreviewMultiPortRoutesAnyPort(t *testing.T) {
	setFastWakeTimeout(t)
	fake := runtimetest.NewFake()
	fake.MultiPort = true
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	if tgt, err := d.PreviewTarget(ctx, sb.ID, 8000, "default"); err != nil || tgt == "" {
		t.Fatalf("primary port 8000: tgt=%q err=%v", tgt, err)
	}
	tgt, err := d.PreviewTarget(ctx, sb.ID, 3000, "default")
	if err != nil {
		t.Fatalf("multi-port 3000 rejected: %v (want route to guest:3000)", err)
	}
	if tgt == "" || tgt[len(tgt)-4:] != "3000" {
		t.Fatalf("port 3000 routed to %q, want an address ending in :3000", tgt)
	}
}

func TestPreviewSinglePortRejectsOtherPort(t *testing.T) {
	setFastWakeTimeout(t)
	fake := runtimetest.NewFake() // MultiPort defaults to false
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t"})
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 3000, "default"); !errors.Is(err, rt.ErrNoAppOnPort) {
		t.Fatalf("single-port other-port err = %v, want ErrNoAppOnPort", err)
	}
}

// The terminal and agent ports are the platform's, not the customer's app, so
// the single-port check must exempt them.
func TestPlatformPortsAreReachableAlongsideADeclaredApp(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "base"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.StartServer(ctx, sb.ID, "true", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	for _, port := range []int{rt.TerminalPort, rt.AgentPort} {
		if _, err := d.PreviewTarget(ctx, sb.ID, port, "default"); errors.Is(err, rt.ErrNoAppOnPort) {
			t.Errorf("port %d refused as 'no app listening' — that is the platform's own "+
				"terminal/agent, so the tab is unreachable for every workstation that "+
				"declares an app port", port)
		}
	}

	// The exemption must not weaken the check for arbitrary ports.
	if _, err := d.PreviewTarget(ctx, sb.ID, 3000, "default"); !errors.Is(err, rt.ErrNoAppOnPort) {
		t.Errorf("port 3000 was allowed through: got %v, want ErrNoAppOnPort", err)
	}
}
