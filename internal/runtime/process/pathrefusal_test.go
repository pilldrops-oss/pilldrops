package process

import (
	"errors"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Every runtime refuses a path outside the workspace, and each words it
// differently on purpose — a vague refusal is what keeps an escape attempt and
// a typo indistinguishable from outside. The API still has to tell that
// refusal from a node fault without reading anyone's prose: it used to answer
// 503 "retry" for what is the caller's own input and is refused identically
// every time.
func TestAPathEscapeIsAPathRefusal(t *testing.T) {
	f := &workdirFS{root: t.TempDir()}
	if _, err := f.resolve("../../etc/passwd"); !errors.Is(err, rt.ErrPathRefused) {
		t.Fatalf("an escape is not classified as a refused path: %v", err)
	}
	if _, err := f.resolve("sub/ok.txt"); err != nil {
		t.Errorf("a path inside the workspace was refused: %v", err)
	}
}
