package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// TestConcurrentLifecycle hammers the lifecycle from many goroutines; must be clean under -race.
func TestConcurrentLifecycle(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	defer st.Close()
	d := New(Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snaps"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := sb.ID

	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				switch (g + i) % 6 {
				case 0:
					_ = d.Pause(ctx, id)
				case 1:
					_ = d.Resume(ctx, id)
				case 2:
					_, _ = d.Exec(ctx, id, rt.ExecSpec{Cmd: "true"})
				case 3:
					_, _ = d.Get(id)
				case 4:
					d.Touch(id)
				case 5:
					_, _ = d.PreviewTarget(ctx, id, 8000, "default")
				}
			}
		}(g)
	}
	wg.Wait()

	if err := d.Resume(ctx, id); err != nil {
		t.Fatalf("final resume: %v", err)
	}
	res, err := d.Exec(ctx, id, rt.ExecSpec{Cmd: "echo ok"})
	if err != nil || res.Stdout != "ok\n" {
		t.Fatalf("final exec = %+v, %v", res, err)
	}

	// Concurrent destroys must not panic or double-free.
	var dwg sync.WaitGroup
	for k := 0; k < 5; k++ {
		dwg.Add(1)
		go func() { defer dwg.Done(); _ = d.Destroy(ctx, id) }()
	}
	dwg.Wait()
	if _, ok := d.Get(id); ok {
		t.Fatalf("sandbox should be gone after destroy")
	}
}
