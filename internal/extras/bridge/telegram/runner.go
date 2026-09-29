package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentcmd"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// Sandboxes is the daemon surface the runner needs.
type Sandboxes interface {
	Get(id string) (SandboxState, bool)
	Resume(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, spec rt.ExecSpec) (rt.ExecResult, error)
}

// SandboxState is the slice of sandbox state the runner cares about.
type SandboxState struct {
	Template string
	Paused   bool
}

// DaemonRunner runs a chat task on a workstation: wake it, then hand the task
// to whichever agent that template ships.
type DaemonRunner struct {
	Boxes Sandboxes
	Logf  func(string, ...any)
	// Turns routes the task through agentd. Nil, or a sandbox that cannot serve it, uses the exec path.
	Turns AgentTurns
	// Gate serializes turns per workstation. Nil means the process-wide gate; NoGate when the caller already holds the slot.
	Gate *SandboxGate
}

// Run implements Runner.
func (r *DaemonRunner) Run(ctx context.Context, sandboxID, task string, opts RunOpts) (string, error) {
	// Concurrent turns corrupt the agent's own transcript.
	if !r.Gate.acquire(sandboxID) {
		return "", ErrBusy
	}
	defer r.Gate.release(sandboxID)

	st, ok := r.Boxes.Get(sandboxID)
	if !ok {
		return "", fmt.Errorf("that workstation no longer exists — link a new one")
	}
	if st.Paused {
		if err := r.Boxes.Resume(ctx, sandboxID); err != nil {
			return "", fmt.Errorf("couldn't wake the workstation: %w", err)
		}
	}

	if r.Turns != nil {
		reply, err := r.Turns.RunTurn(ctx, sandboxID, task, opts)
		if err == nil {
			return reply, nil
		}
		if !errors.Is(err, ErrTurnsUnavailable) {
			// Retrying a refusal (spend cap, full queue) on the exec path would bypass the limit that refused it.
			return "", err
		}
		if r.Logf != nil {
			r.Logf("sandbox %s cannot serve turns through agentd (%v) — using the exec path", sandboxID, err)
		}
	}

	cmd, err := AgentCommand(st.Template, task, opts)
	if err != nil {
		return "", err
	}
	res, err := r.Boxes.Exec(ctx, sandboxID, rt.ExecSpec{
		Cmd: cmd,
		// Inner guard so a wedged agent cannot hold the sandbox if the outer context is lost.
		Timeout: 15 * time.Minute,
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 {
		// A failing agent usually explains why on stderr.
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = out
		}
		return "", fmt.Errorf("the agent exited %d: %s", res.ExitCode, Truncate(detail, 500))
	}
	if out == "" {
		out = strings.TrimSpace(res.Stderr)
	}
	return out, nil
}

// AgentCommand builds the shell command that runs task on the template's agent.
// FollowUp selects ContinueCmd when the agent has one; Autonomous appends the agent's own --dangerously-* flags.
func AgentCommand(template, task string, opts RunOpts) (string, error) {
	tmpl, ok := templates.Resolve(template)
	if !ok || tmpl.Agent == nil || tmpl.Agent.RunCmd == "" {
		return "", fmt.Errorf("this workstation isn't running a chat-capable agent")
	}
	pattern := tmpl.Agent.RunCmd
	if opts.FollowUp && tmpl.Agent.ContinueCmd != "" {
		pattern = tmpl.Agent.ContinueCmd
	}
	cmd := substituteTask(pattern, task)
	if opts.Autonomous && tmpl.Agent.RunFlags != "" {
		// Flags go before the task argument or the agent treats them as prompt text.
		cmd = insertFlags(cmd, tmpl.Agent.CLIBin, tmpl.Agent.RunFlags)
		// Claude Code refuses --dangerously-skip-permissions as root unless IS_SANDBOX=1.
		if env := tmpl.Agent.RunFlagsEnv; env != "" {
			cmd = env + " " + cmd
		}
	}
	return cmd, nil
}

func insertFlags(cmd, bin, flags string) string { return agentcmd.InsertFlags(cmd, bin, flags) }

// substituteTask, shellQuote and insertFlags delegate to agentcmd so there is
// exactly one shell-quoting implementation shared with agentd.
func substituteTask(pattern, task string) string { return agentcmd.Substitute(pattern, task) }

func shellQuote(s string) string { return agentcmd.Quote(s) }
