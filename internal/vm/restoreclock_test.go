package vm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every way a guest comes out of a memory image has to step its wall clock:
// there is no NTP, no RTC and no hypervisor time device inside, so a machine
// restored from a week-old image believes it is last week and signs, verifies
// and timestamps accordingly. The bug this pins is a path that forgot the push,
// which is invisible from the outside until a certificate fails.
//
// attachRestored is the funnel for a resume, a fork and a create from a named
// snapshot; RestoreFromSnapshot is the template golden warm boot. A restore
// path that routes through neither would need its own push.
func TestEveryRestoreFunnelStepsTheGuestClock(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"attachRestored": false, "RestoreFromSnapshot": false}

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, tracked := want[fn.Name.Name]; !tracked {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "deliverGuestClock" {
						want[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}

	for name, found := range want {
		if !found {
			t.Errorf("%s does not call deliverGuestClock — a guest restored through it "+
				"keeps the wall clock the memory image was written with", name)
		}
	}
}
