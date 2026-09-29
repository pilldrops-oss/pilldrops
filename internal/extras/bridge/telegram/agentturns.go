package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Turns route through agentd so they hit the queue, journal, spend cap and
// approval gate. A sandbox that cannot serve a turn returns ErrTurnsUnavailable
// and the runner falls back to the direct exec path.

// ErrTurnsUnavailable means the sandbox cannot serve the task through agentd; callers fall back to exec.
var ErrTurnsUnavailable = errors.New("agentd turns are not available for this sandbox")

// AgentTurns submits a task as a queued turn and waits for the reply.
type AgentTurns interface {
	RunTurn(ctx context.Context, sandboxID, task string, opts RunOpts) (string, error)
}

// turnPoll is how often RunTurn checks for completion; a var so tests can shorten it.
var turnPoll = 2 * time.Second

// turnAwaitingGrace is how long to wait on a parked approval before answering.
// The bridge cannot render an approval prompt, so the reply names what is parked instead.
var turnAwaitingGrace = 45 * time.Second

// RunTurn implements AgentTurns over the platform HTTP API.
func (a *APIBoxes) RunTurn(ctx context.Context, sandboxID, task string, opts RunOpts) (string, error) {
	mode := "autonomous"
	if !opts.Autonomous {
		ok, err := a.guidedAvailable(ctx, sandboxID)
		if err != nil {
			return "", ErrTurnsUnavailable
		}
		if !ok {
			return "", ErrTurnsUnavailable
		}
		mode = "guided"
	}

	thread := "latest"
	if !opts.FollowUp {
		thread = "new"
	}

	var sub struct {
		TurnID string `json:"turn_id"`
		State  string `json:"state"`
	}
	err := a.do(ctx, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/agent/turns", map[string]any{
		"message": task,
		"mode":    mode,
		"thread":  thread,
		// The machine sleeps between messages and resumes with its transcript intact.
		"wake": true,
	}, &sub)
	if err != nil {
		// Only a missing agent surface falls back. A spend cap or full queue must
		// not be retried on a path that does not enforce it.
		if turnsUnsupported(err) {
			return "", ErrTurnsUnavailable
		}
		return "", err
	}
	if sub.TurnID == "" {
		return "", ErrTurnsUnavailable
	}
	return a.waitForTurn(ctx, sandboxID, sub.TurnID)
}

// guidedAvailable reports whether this sandbox offers Guided mode.
func (a *APIBoxes) guidedAvailable(ctx context.Context, sandboxID string) (bool, error) {
	var out struct {
		Session struct {
			Autonomy struct {
				Available []string `json:"available"`
			} `json:"autonomy"`
		} `json:"session"`
	}
	if err := a.do(ctx, http.MethodGet, "/v1/sandboxes/"+sandboxID+"/agent/status", nil, &out); err != nil {
		return false, err
	}
	for _, m := range out.Session.Autonomy.Available {
		if m == "guided" {
			return true, nil
		}
	}
	return false, nil
}

// waitForTurn polls until the turn ends and returns its reply.
func (a *APIBoxes) waitForTurn(ctx context.Context, sandboxID, turnID string) (string, error) {
	path := "/v1/sandboxes/" + sandboxID + "/agent/turns/" + turnID
	var awaitingSince time.Time
	for {
		select {
		case <-ctx.Done():
			// The bridge's TaskTimeout hit; the turn stays queued and its reply lands in the transcript.
			//nolint:staticcheck // ST1005: this is verbatim user-facing chat copy
			return "", fmt.Errorf("I'm still working on that — it's taking longer than this " +
				"chat will wait. The task is still running; ask me again in a minute and I'll " +
				"tell you where it got to")
		case <-time.After(turnPoll):
		}

		var t struct {
			State    string `json:"state"`
			Response string `json:"response"`
			Error    string `json:"error"`
		}
		if err := a.do(ctx, http.MethodGet, path, nil, &t); err != nil {
			return "", err
		}

		if t.State == "awaiting_approval" {
			if awaitingSince.IsZero() {
				awaitingSince = time.Now()
			} else if time.Since(awaitingSince) > turnAwaitingGrace {
				return a.parkedMessage(ctx, sandboxID, turnID), nil
			}
			continue
		}
		awaitingSince = time.Time{}

		switch t.State {
		case "done":
			return t.Response, nil
		case "failed":
			if t.Error != "" {
				return "", errors.New(t.Error)
			}
			return "", errors.New("the agent didn't finish that successfully")
		case "interrupted":
			// Partial output is still worth reporting.
			if t.Response != "" {
				return t.Response, nil
			}
			return "", errors.New("that turn was interrupted")
		}
	}
}

// parkedMessage names the command awaiting approval, so the user is not asked to consent to something unseen.
func (a *APIBoxes) parkedMessage(ctx context.Context, sandboxID, turnID string) string {
	var out struct {
		Approvals []struct {
			TurnID    string `json:"turn_id"`
			Signature string `json:"signature"`
			Tool      string `json:"tool"`
		} `json:"approvals"`
	}
	what := ""
	if err := a.do(ctx, http.MethodGet, "/v1/sandboxes/"+sandboxID+"/agent/approvals", nil, &out); err == nil {
		for _, p := range out.Approvals {
			// Only this turn's park; another turn's command is not what the user was shown.
			if p.TurnID != "" && p.TurnID != turnID {
				continue
			}
			if what = p.Signature; what == "" {
				what = p.Tool
			}
			if what != "" {
				break
			}
		}
	}
	if what == "" {
		return "I've paused: what I need to do next is outside what I'm allowed to do on my " +
			"own. Approve it on the workstation's Agent tab and I'll carry on — your task is " +
			"still queued."
	}
	return "I need your approval to run `" + what + "` before I can carry on. Approve it on the " +
		"workstation's Agent tab; your task stays queued until you do."
}

// turnsUnsupported reports whether an error means the sandbox has no agent
// turns API, as opposed to a refusal the caller should be shown.
func turnsUnsupported(err error) bool {
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		"agent_not_supported", // not an agent template, or no agent surface
		"sandbox not found",   // an older node without these routes answers this way
		"404",
		"not found",
		"no agent",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}
