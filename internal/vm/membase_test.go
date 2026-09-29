//go:build linux

package vm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func baseManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{config: Config{OverlayDir: t.TempDir()}, log: zap.NewNop()}
}

// The capture a VM is restored from belongs to whoever made it: `checkpoint rm`
// removes one on request, a fork drops its source, a backup clears its staging.
// A VM whose dirty-page base is a file inside one of those directories loses it
// without warning, and then every pause, fork and snapshot of a perfectly
// healthy sandbox fails on a missing file.
func TestTheMemoryBaseOutlivesTheCaptureItCameFrom(t *testing.T) {
	m := baseManager(t)
	capture := filepath.Join(t.TempDir(), "sb-1.ckpt", "k1")
	if err := os.MkdirAll(capture, 0o755); err != nil {
		t.Fatal(err)
	}
	mem := filepath.Join(capture, "mem")
	if err := os.WriteFile(mem, []byte("guest memory"), 0o600); err != nil {
		t.Fatal(err)
	}

	base, err := m.adoptMemBase("sb-1", mem)
	if err != nil {
		t.Fatal(err)
	}
	if base == mem {
		t.Fatalf("the base is still the capture's own file (%s); deleting the capture takes it with it", base)
	}
	if err := os.RemoveAll(filepath.Dir(capture)); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("memory base gone with the capture that made it: %v", err)
	}
	if string(got) != "guest memory" {
		t.Errorf("memory base reads %q, want the captured image", got)
	}
}

// One name per sandbox: a base that is replaced has to be dropped in the same
// step, or every capture pins another whole memory image on the node's disk.
func TestAdoptingABaseDropsTheOneItReplaces(t *testing.T) {
	m := baseManager(t)
	src := t.TempDir()
	for _, name := range []string{"first", "second"} {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.adoptMemBase("sb-1", p); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(m.lineageDir("sb-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("lineage holds %v; a replaced base must not be left behind", names)
	}
	got, err := os.ReadFile(filepath.Join(m.lineageDir("sb-1"), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("tracked base is %q, want the one just adopted", got)
	}
}

// attachRestored is the one funnel a resume, a fork, a checkpoint restore and a
// create-from-snapshot all come out of, and the path each was handed is inside
// somebody else's directory. Pinned by shape because the alternative needs a
// live guest: a restore that recorded the caller's path directly is what made
// `checkpoint save; checkpoint restore; checkpoint rm` break every later memory
// operation on the sandbox.
func TestAttachRestoredTakesItsOwnLinkToTheMemoryBase(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue // a build-tagged file this test does not care about
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "attachRestored" {
				continue
			}
			found := false
			ast.Inspect(fn, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "adoptMemBase" {
						found = true
					}
				}
				return true
			})
			if !found {
				t.Fatal("attachRestored does not call adoptMemBase — a sandbox restored through it " +
					"tracks dirty pages against a file its caller is free to delete")
			}
			return
		}
	}
	t.Fatal("attachRestored not found")
}
