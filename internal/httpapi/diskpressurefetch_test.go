package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// pressuredRuntime is an ordinary runtime that says its filesystem is full.
type pressuredRuntime struct {
	rt.Runtime
	full atomic.Bool
}

func (p *pressuredRuntime) DiskPressured() bool { return p.full.Load() }

// The engine refuses a create, a fork, a checkpoint and a boot-from-capture on
// a full filesystem — and every one of those guards sits below the template
// fetch. The fetch is the bigger write: a named snapshot materialises a memory
// image, a machine state and a disk out of the artifact store before the boot
// copies all three again. Measured on a full 8 GB filesystem, a create from a
// 1.6 GB snapshot was refused only after the fetch had already run the disk out
// of space and left a partial capture behind.
func TestACreateOnAFullDiskIsRefusedBeforeItFetchesAnything(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "test", "default")
	if err != nil {
		t.Fatal(err)
	}
	engine := &pressuredRuntime{Runtime: process.New(filepath.Join(dir, "sb"))}
	d := daemon.New(daemon.Config{
		Runtime:     engine,
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	srv := httptest.NewServer(New(d, st).Routes())
	t.Cleanup(srv.Close)

	// A store tier that counts, so "refused before the fetch" is a measurement
	// rather than an inference from the status code.
	var fetches atomic.Int64
	templates.SetDir(dir)
	templates.SetSource(func(context.Context, string, string) error {
		fetches.Add(1)
		return errors.New("the artifact store should never have been asked")
	})
	t.Cleanup(func() { templates.SetSource(nil); templates.SetDir("") })

	engine.full.Store(true)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"held-only-in-the-store"}`)
	if code != http.StatusServiceUnavailable || !strings.Contains(str(out, "error"), "disk pressure") {
		t.Errorf("create on a full disk = %d %q, want 503 naming disk pressure", code, str(out, "error"))
	}
	if n := fetches.Load(); n != 0 {
		t.Errorf("the artifact store was asked %d times on a full disk: the fetch is the largest "+
			"write the node makes and it runs above every guard in the engine", n)
	}

	// Positive control, both halves: with the disk free the same create reaches
	// the fetch, so the refusal above was the guard and not a broken route.
	engine.full.Store(false)
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"held-only-in-the-store"}`)
	if fetches.Load() == 0 {
		t.Errorf("with the disk free the create still never reached the fetch (= %d %q): "+
			"the guard is stuck on", code, str(out, "error"))
	}
	if code == http.StatusServiceUnavailable && strings.Contains(str(out, "error"), "disk pressure") {
		t.Error("the create still reports disk pressure once the disk is free")
	}
}
