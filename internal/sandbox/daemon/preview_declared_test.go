package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// A single-port runtime publishes the declared ports at boot too, so the guard
// must admit them and still refuse everything else.
func TestSinglePortAdmitsDeclaredPorts(t *testing.T) {
	setFastWakeTimeout(t)
	fake := runtimetest.NewFake() // MultiPort false
	d := New(Config{Runtime: fake, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()

	sb, _ := d.Create(ctx, CreateSpec{Template: "t", Ports: []int{5432}})
	if err := d.StartServer(ctx, sb.ID, "true", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 5432, "default"); errors.Is(err, rt.ErrNoAppOnPort) {
		t.Fatalf("declared port 5432 refused: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 3000, "default"); !errors.Is(err, rt.ErrNoAppOnPort) {
		t.Fatalf("undeclared port 3000 err = %v, want ErrNoAppOnPort (control)", err)
	}
	// The catalogue's declared ports count even when the request listed none.
	pg, _ := d.Create(ctx, CreateSpec{Template: "postgres"})
	if err := d.StartServer(ctx, pg.ID, "true", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, pg.ID, 5432, "default"); errors.Is(err, rt.ErrNoAppOnPort) {
		t.Fatalf("catalogue-declared port 5432 refused: %v", err)
	}
}
