package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func TestIntegrationProcessRuntime(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	pr := process.New(filepath.Join(dir, "sandboxes"))
	d := New(Config{
		Runtime: pr, Store: StoreAdapter{S: st},
		IdleTimeout: time.Minute, HardTTL: time.Hour,
	})
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "echo integration"})
	if err != nil || res.Stdout != "integration\n" {
		t.Fatalf("exec = %+v, %v", res, err)
	}

	fs, err := d.FS(sb.ID)
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	if err := fs.Write(t.Context(), "data.txt", []byte("persisted")); err != nil {
		t.Fatalf("fs write: %v", err)
	}
	res, _ = d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "cat data.txt"})
	if res.Stdout != "persisted" {
		t.Fatalf("cat = %q, want persisted", res.Stdout)
	}

	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State != StatePaused {
		t.Fatalf("state = %s, want paused", got.State)
	}
	// No resume call: the exec wakes the sandbox and reads the persisted file.
	res, err = d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "cat data.txt"})
	if err != nil || res.Stdout != "persisted" {
		t.Fatalf("post-pause cat = %q, %v; want persisted (wake-on-exec or snapshot roundtrip failed)", res.Stdout, err)
	}
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state after exec = %s, want running", got.State)
	}

	recs, err := st.LoadSandboxes(t.Context())
	if err != nil || len(recs) != 1 || recs[0].ID != sb.ID {
		t.Fatalf("store records = %+v, %v", recs, err)
	}

	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}
