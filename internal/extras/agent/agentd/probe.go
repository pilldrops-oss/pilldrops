package agentd

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Boot-time probe: does the CLI still accept its undocumented permission-prompt
// flag? Whether the gate is actually consulted is audited by the supervisor on real turns.

// probeTimeout bounds each probe invocation.
const probeTimeout = 15 * time.Second

// ProbeResult is the probe's conclusion.
type ProbeResult struct {
	// Accepted is false only on positive evidence that the flag was rejected.
	Accepted bool
	// Inconclusive means the CLI is missing or failed for reasons unrelated to the flag.
	Inconclusive bool
	Reason       string
}

// ProbePermissionTool reports whether the agent CLI accepts its permission-prompt
// flag. It runs --version with and without the flag; only "plain works, flag
// fails" counts as rejection, everything else is inconclusive.
func ProbePermissionTool(ctx context.Context, shell, cliBin, flagPattern string) ProbeResult {
	if cliBin == "" || flagPattern == "" {
		return ProbeResult{Inconclusive: true, Reason: "no agent binary or permission flag to probe"}
	}
	if shell == "" {
		shell = "/bin/sh"
	}

	run := func(args string) error {
		c, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		cmd := exec.CommandContext(c, shell, "-c", cliBin+" "+args)
		return cmd.Run()
	}

	if err := run("--version"); err != nil {
		return ProbeResult{
			Inconclusive: true,
			Reason:       "the agent CLI did not answer --version, so the flag could not be probed: " + err.Error(),
		}
	}

	// The tool name need not exist; an unknown option is rejected before its value is read.
	flag := strings.ReplaceAll(flagPattern, "{tool}", "mcp__probe__probe")
	if err := run(flag + " --version"); err != nil {
		return ProbeResult{
			Accepted: false,
			Reason: "this build of the agent CLI rejects " + flagPattern +
				", so tool calls cannot be intercepted for review",
		}
	}
	return ProbeResult{Accepted: true}
}
