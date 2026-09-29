// Package agentcmd builds the shell command that runs a task on an agent CLI.
// It is the single implementation of that quoting, shared by the chat bridge
// on the orchestrator and by agentd inside the sandbox.
package agentcmd

import "strings"

// Quote wraps s as a POSIX single-quoted literal.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Substitute puts task into a command pattern as a single-quoted literal,
// replacing the pattern's own quotes around the placeholder so nothing nests.
// A pattern with no placeholder gets the task appended.
func Substitute(pattern, task string) string {
	q := Quote(task)
	// Quoted forms first, so no stray pair of quotes is left behind.
	for _, ph := range []string{`"{task}"`, `'{task}'`, `{task}`} {
		if strings.Contains(pattern, ph) {
			return strings.ReplaceAll(pattern, ph, q)
		}
	}
	return pattern + " " + q
}

// InsertFlags places flags immediately after the binary name, since an agent
// that treats everything after its prompt as prompt text would swallow them.
func InsertFlags(cmd, bin, flags string) string {
	if flags == "" {
		return cmd
	}
	if bin == "" || !strings.HasPrefix(cmd, bin+" ") {
		return cmd + " " + flags
	}
	return bin + " " + flags + strings.TrimPrefix(cmd, bin)
}
