package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

type pinRecorder struct {
	mu       sync.Mutex
	pinned   []string
	released []string
}

func (p *pinRecorder) hook(template string) func() {
	p.mu.Lock()
	p.pinned = append(p.pinned, template)
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.released = append(p.released, template)
		p.mu.Unlock()
	}
}

func (p *pinRecorder) snapshot() (pinned, released []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pinned...), append([]string(nil), p.released...)
}

func TestCreatePinsTheTemplateAndDestroyReleasesIt(t *testing.T) {
	rec := &pinRecorder{}
	d := New(Config{
		Runtime:     runtimetest.NewFake(),
		IdleTimeout: time.Minute,
		HardTTL:     time.Hour,
		PinTemplate: rec.hook,
	})
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "code-interpreter", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pinned, released := rec.snapshot()
	if len(pinned) != 1 || pinned[0] != "code-interpreter" {
		t.Fatalf("create pinned %v, want [code-interpreter] — without the pin, eviction is "+
			"free to remove the rootfs this VM is booted from", pinned)
	}
	if len(released) != 0 {
		t.Fatalf("the pin was released while the sandbox was still running: %v", released)
	}

	// Pause must not release: the sandbox resumes from the same rootfs.
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, released := rec.snapshot(); len(released) != 0 {
		t.Errorf("pausing released the template pin (%v) — a paused sandbox is resumed from "+
			"the same artifact, so it can now be evicted before the resume", released)
	}

	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, released := rec.snapshot(); len(released) != 1 || released[0] != "code-interpreter" {
		t.Fatalf("destroy released %v, want [code-interpreter] — the pin is never released and "+
			"the node's cache fills with the template of every sandbox it has ever run", released)
	}
}

func TestAFailedBootReleasesTheTemplatePin(t *testing.T) {
	rec := &pinRecorder{}
	fake := runtimetest.NewFake()
	fake.BootErr = errors.New("no capacity")
	d := New(Config{
		Runtime:     fake,
		IdleTimeout: time.Minute,
		HardTTL:     time.Hour,
		PinTemplate: rec.hook,
	})

	if _, err := d.Create(context.Background(), CreateSpec{Template: "base", OrgID: "default"}); err == nil {
		t.Fatal("precondition: the boot was supposed to fail")
	}
	pinned, released := rec.snapshot()
	if len(pinned) != 1 {
		t.Fatalf("pinned %v, want one pin taken before the boot", pinned)
	}
	if len(released) != 1 {
		t.Errorf("a create that failed to boot left its template pinned (%v released) — the "+
			"artifact stays resident with nothing running on it", released)
	}
}

// A nil hook is the ordinary case on a docker or process node.
func TestNoPinHookIsNotACrash(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "base", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}
