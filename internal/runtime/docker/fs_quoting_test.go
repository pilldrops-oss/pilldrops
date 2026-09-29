package docker

import (
	"fmt"
	"strings"
	"testing"
)

// hostilePaths are shapes a shell would interpret if the path were interpolated.
var hostilePaths = []string{
	`x$(touch /tmp/pwned)y`,
	"x`touch /tmp/pwned`y",
	`x";touch /tmp/pwned;"y`,
	`x'y`,
	`x\$y`,
	"x\ny",
	`x y`,
	`x*y`,
	`--rf`, // leading dashes must not be read as flags
}

// TestGoQuotingIsNotShellQuoting shows %q leaves shell metacharacters live, so
// nobody reintroduces Sprintf("%q") as shell quoting.
func TestGoQuotingIsNotShellQuoting(t *testing.T) {
	for _, p := range []string{`x$(id)y`, "x`id`y"} {
		quoted := fmt.Sprintf("%q", p)
		if !strings.Contains(quoted, "$(") && !strings.Contains(quoted, "`") {
			t.Fatalf("%%q on %q produced %s — test premise is wrong", p, quoted)
		}
		if strings.Contains(quoted, `\$`) || strings.Contains(quoted, "\\`") {
			t.Errorf("%%q unexpectedly escaped the shell metacharacter in %s", quoted)
		}
	}
}

func TestPathsArePassedAsArgvNotShellText(t *testing.T) {
	d := &dockerFS{cid: "test-cid"}
	for _, p := range hostilePaths {
		abs := d.abs(p)

		for name, argv := range map[string][]string{
			"Remove": {"rm", "-rf", "--", abs},
			"Mkdir":  {"mkdir", "-p", "--", abs},
			"Stat":   {"stat", "-c", "%s|%F|%Y|%a", "--", abs},
			"Read":   {"cat", "--", abs},
		} {
			found := false
			for _, el := range argv {
				if el == abs {
					found = true
				}
				if el != abs && strings.Contains(el, p) {
					t.Errorf("%s: path %q was interpolated into argv element %q", name, p, el)
				}
			}
			if !found {
				t.Errorf("%s: resolved path %q is not a standalone argv element", name, abs)
			}
		}
	}
}

func TestShellScriptsUsePositionalParameters(t *testing.T) {
	for name, script := range map[string]string{
		"listScript":  listScript,
		"writeScript": writeScript,
		"moveScript":  moveScript,
	} {
		if !strings.Contains(script, `"$1"`) {
			t.Errorf("%s does not reference its path as \"$1\" — it must not interpolate", name)
		}
		// %q means Sprintf is back. %s and %Y are legitimate printf and stat verbs, so not checked.
		if strings.Contains(script, "%q") {
			t.Errorf("%s contains %%q; paths must arrive as positional parameters, not Sprintf arguments", name)
		}
		// A leftover Sprintf-doubled %%Y makes stat print a literal "%Y" and every mtime read as 0.
		if strings.Contains(script, "%%") {
			t.Errorf("%s still has Sprintf-doubled %%%% verbs; it is no longer a format string", name)
		}
	}
}

func TestAbsClampsRelativePathsAndTraversal(t *testing.T) {
	d := &dockerFS{cid: "c"}
	for in, want := range map[string]string{
		"app.py":        containerWorkdir + "/app.py",
		"./app.py":      containerWorkdir + "/app.py",
		"a/../b.py":     containerWorkdir + "/b.py",
		"../../etc/pw":  containerWorkdir + "/etc/pw", // traversal collapsed, not escaped
		"/tmp/shot.png": "/tmp/shot.png",              // absolute passes through by design
	} {
		if got := d.abs(in); got != want {
			t.Errorf("abs(%q) = %q, want %q", in, got, want)
		}
	}
}
