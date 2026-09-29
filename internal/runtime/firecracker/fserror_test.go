package firecracker

import (
	"errors"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// The filesystem verbs are implemented by running find(1), stat(1), mkdir(1),
// mv(1) and rm(1) in the guest, and their stderr used to be handed to the
// caller whole. `fs ls <id> nope` answered
//
//	command failed (exit 1): find: 'nope': No such file or directory
//
// as a 404, and the CLI's hint for a 404 sent the caller to check the sandbox
// id — the one thing that had been right. The tool's name is an implementation
// detail of this file; the missing path is what the caller needs.
func TestAMissingPathIsAMissingPathAndSaysNothingAboutFind(t *testing.T) {
	err := fsError("nope", "find: ‘nope’: No such file or directory\n", 1)
	if !errors.Is(err, rt.ErrPathNotFound) {
		t.Fatalf("a missing path is not classified as one: %v", err)
	}
	// Not ErrNotFound: that one means the sandbox, and reporting a mistyped
	// path as a missing sandbox is what sent people to `sandbox ls`.
	if errors.Is(err, rt.ErrNotFound) {
		t.Error("a missing path reads as a missing sandbox")
	}
	if strings.Contains(err.Error(), "find") {
		t.Errorf("the implementation's tool leaks to the caller: %q", err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the refusal does not name the path that was wrong: %q", err)
	}
}

// Anything else still reaches the caller — losing the guest's own words would
// be worse than the prefix ever was — but without the tool's name in front.
func TestOtherFailuresKeepTheirMessageAndLoseTheToolName(t *testing.T) {
	err := fsError("/etc/shadow", "stat: cannot read: Permission denied\n", 1)
	if errors.Is(err, rt.ErrPathNotFound) {
		t.Fatal("a permission failure classified as a missing path; the caller would be told to check the spelling")
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "stat:") || strings.Contains(msg, " stat:") {
		t.Errorf("tool name still in front: %q", msg)
	}
	for _, want := range []string{"Permission denied", "/etc/shadow"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q is missing from %q", want, msg)
		}
	}
}

// envd confines every file path to the sandbox home and refuses the rest with
// one deliberately generic "invalid path". Relayed as an opaque rpc error, that
// reached the caller as `invalid path (HTTP 503)` with the hint "server-side
// error — retry": the caller's own path, reported as the node being unwell,
// with advice that can never succeed.
func TestTheGuestsPathRefusalIsClassifiedAsTheCallersInput(t *testing.T) {
	err := pathRefusal("/tmp/x", errors.New("rpc error: code = Unknown desc = invalid path"))
	if !errors.Is(err, rt.ErrPathRefused) {
		t.Fatalf("a refused path is not classified as one: %v", err)
	}
	if !strings.Contains(err.Error(), "/tmp/x") {
		t.Errorf("the refusal does not name the path: %q", err)
	}
	// Whether the target is there is not something a refusal may reveal: that
	// is why envd's own message is vague, and nothing here may un-vague it.
	if strings.Contains(strings.ToLower(err.Error()), "no such") {
		t.Errorf("the refusal leaks whether the target exists: %q", err)
	}

	// Everything else keeps its own meaning. Classifying a transport failure as
	// the caller's input would turn a node outage into a 400 nobody retries.
	other := errors.New("rpc error: code = Unavailable desc = connection refused")
	if got := pathRefusal("/tmp/x", other); !errors.Is(got, other) || errors.Is(got, rt.ErrPathRefused) {
		t.Errorf("an unrelated failure was reclassified: %v", got)
	}
	if pathRefusal("/tmp/x", nil) != nil {
		t.Error("success turned into an error")
	}
}

// A helper that fails with nothing on stderr still has to say something.
func TestASilentFailureStillReportsSomething(t *testing.T) {
	msg := fsError("x", "", 2).Error()
	if !strings.Contains(msg, "2") || !strings.Contains(msg, "x") {
		t.Errorf("silent failure reported as %q; it names neither the path nor the code", msg)
	}
}
