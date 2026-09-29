package main

import "testing"

// envd's Execute RPC carries no environment, so variables ride in the command
// text and quoting is a security property.
func TestWithBootEnvQuotesHostileValues(t *testing.T) {
	// No env must not wrap the command, since wrapping changes $VAR expansion.
	if got := withBootEnv(nil, "echo hi"); got != "echo hi" {
		t.Errorf("withBootEnv(nil) = %q, want the command unchanged", got)
	}

	// A value carrying a quote and a command separator must survive as data.
	got := withBootEnv([]string{`EVIL=a'b; rm -rf /;#`}, "printenv EVIL")
	want := `env 'EVIL=a'\''b; rm -rf /;#' sh -c 'printenv EVIL'`
	if got != want {
		t.Errorf("withBootEnv escaped badly:\n got %s\nwant %s", got, want)
	}
}

func TestShellQuoteDev(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`plain`, `'plain'`},
		{``, `''`},
		{`a'b`, `'a'\''b'`},
		{`'`, `''\'''`},
		{`; rm -rf /`, `'; rm -rf /'`},
		{`$(whoami)`, `'$(whoami)'`},
		{"nl\nhere", "'nl\nhere'"},
	} {
		if got := shellQuoteDev(tc.in); got != tc.want {
			t.Errorf("shellQuoteDev(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// Without the inner `sh -c` the outer shell expands $VAR before env runs.
func TestWithBootEnvKeepsVariableExpansionWorking(t *testing.T) {
	got := withBootEnv([]string{"TOKEN=abc"}, "echo $TOKEN")
	if want := `env 'TOKEN=abc' sh -c 'echo $TOKEN'`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
