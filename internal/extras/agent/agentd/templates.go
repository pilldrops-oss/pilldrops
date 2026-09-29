package agentd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentcmd"
)

// Templates turns a turn into a shell command from the patterns agentd was
// launched with; agentd itself does not know which agent it runs.
type Templates struct {
	// RunCmd starts a fresh conversation. Required.
	RunCmd string
	// ContinueCmd continues the latest conversation. Empty means every turn is a fresh one-shot.
	ContinueCmd string
	// StreamCmd starts a fresh conversation with structured event output.
	StreamCmd string
	// StreamContinueCmd continues a conversation with structured events. Usually empty.
	StreamContinueCmd string

	// ModelsCmd lists the agent's models, one per line. Empty means unsupported.
	ModelsCmd string

	RunFlags    string
	RunFlagsEnv string
	CLIBin      string
	Env         []string

	// PermissionTool is the flag pattern that routes tool approvals to the gate,
	// with {tool} replaced by the gate's MCP tool name. Empty means no Guided mode.
	PermissionTool string
	// GateConfig is the path to the --mcp-config file registering the gate server.
	GateConfig string
	// Shell and Dir run ModelsCmd. Defaults match the Runner's.
	Shell string
	Dir   string
}

// Models lists the agent's models by asking the CLI. Returns errNoModelsCmd when
// there is no listing command.
func (t *Templates) Models(ctx context.Context) ([]string, error) {
	if t.ModelsCmd == "" {
		return nil, errNoModelsCmd
	}
	sh := t.Shell
	if sh == "" {
		sh = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, sh, "-c", t.ModelsCmd)
	cmd.Dir = t.Dir
	cmd.Env = t.Env
	// CommandContext only kills the shell; a forked child still holds the stdout
	// pipe, so Output would block until it exits. WaitDelay closes the pipe.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing models with %q: %w", t.ModelsCmd, err)
	}

	var models []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		m := strings.TrimSpace(line)
		// Skip blanks and header prose like "Available models:".
		if m == "" || strings.HasSuffix(m, ":") || strings.Contains(m, " ") {
			continue
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		models = append(models, m)
		if len(models) >= maxModels {
			break
		}
	}
	return models, nil
}

var errNoModelsCmd = errors.New("this agent cannot list its models")

const (
	// modelsTimeout bounds the listing call, which runs on an API request.
	modelsTimeout = 10 * time.Second
	// maxModels caps the list; OpenCode reports hundreds through OpenRouter.
	maxModels = 500
)

// Build produces the command and environment for a turn.
func (t *Templates) Build(turn *Turn) (string, []string, error) {
	if t.RunCmd == "" {
		return "", nil, fmt.Errorf("no run command configured for this agent")
	}

	pattern, _ := t.pattern(turn)
	cmd := agentcmd.Substitute(pattern, turn.Request)

	switch {
	case turn.Mode == ModeGuided && t.GuidedAvailable():
		// --mcp-config is greedy, so the = form keeps it from swallowing the prompt.
		// --strict-mcp-config blocks servers from the user's own config. RunFlags is
		// not applied: for claude it is --dangerously-skip-permissions, which bypasses the gate.
		flags := "--mcp-config=" + shellQuote(t.GateConfig) + " --strict-mcp-config " +
			strings.ReplaceAll(t.PermissionTool, "{tool}", GatePermissionTool)
		cmd = agentcmd.InsertFlags(cmd, t.CLIBin, flags)

	case turn.Mode == ModeAutonomous && t.RunFlags != "":
		cmd = agentcmd.InsertFlags(cmd, t.CLIBin, t.RunFlags)
		// Claude Code refuses to skip permissions as root unless IS_SANDBOX=1.
		if t.RunFlagsEnv != "" {
			cmd = t.RunFlagsEnv + " " + cmd
		}
	}
	return cmd, t.Env, nil
}

// GuidedAvailable reports whether this agent can run gated.
func (t *Templates) GuidedAvailable() bool {
	return t.PermissionTool != "" && t.GateConfig != ""
}

// shellQuote quotes a path for the shell; a space would otherwise become a
// second --mcp-config path.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`(){}[]*?|&;<>#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pattern picks the template for this turn and reports what was traded away. On a
// follow-up without a stream+continue form, keeping the conversation wins over step detail.
func (t *Templates) pattern(turn *Turn) (pattern string, note string) {
	followUp := turn.FollowUp && t.ContinueCmd != ""
	rich := t.StreamCmd != ""

	switch {
	case followUp && rich && t.StreamContinueCmd != "":
		return t.StreamContinueCmd, ""
	case followUp && rich:
		return t.ContinueCmd, "structured events are unavailable on a follow-up turn for this agent, " +
			"so this turn keeps the conversation but not the step detail"
	case followUp:
		return t.ContinueCmd, ""
	case rich:
		return t.StreamCmd, ""
	default:
		return t.RunCmd, ""
	}
}

// StructuredFor reports whether this turn will emit structured events.
func (t *Templates) StructuredFor(turn *Turn) bool {
	p, _ := t.pattern(turn)
	return p == t.StreamCmd || (t.StreamContinueCmd != "" && p == t.StreamContinueCmd)
}

// DegradeNote returns the trade-off note for a turn, or empty when there is none.
func (t *Templates) DegradeNote(turn *Turn) string {
	_, note := t.pattern(turn)
	return note
}
