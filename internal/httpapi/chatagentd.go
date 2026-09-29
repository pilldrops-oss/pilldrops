//go:build !core

package httpapi

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
)

// chatTurnTimeout bounds how long the blocking chat contract waits.
const chatTurnTimeout = 10 * time.Minute

// chatPollInterval is a var only so tests can shorten it.
var chatPollInterval = 500 * time.Millisecond

// awaitingGrace is how long a blocking chat waits on a parked approval before
// answering. Long enough for a session grant to resolve it, short enough that
// the full approval timeout does not read as a hang.
var awaitingGrace = 30 * time.Second

// errChatAgentdUnavailable means the caller should use the pre-agentd path.
var errChatAgentdUnavailable = errors.New("agentd is not available for this sandbox")

// runChatViaAgentd submits a chat message as a turn and waits for its reply.
// Returns errChatAgentdUnavailable when the caller should fall back to direct exec.
func (s *Server) runChatViaAgentd(ctx context.Context, id, msg string, autonomous bool) (string, error) {
	c, err := s.agentClient(id)
	if err != nil {
		return "", errChatAgentdUnavailable
	}
	st, err := c.Status(ctx)
	if err != nil {
		return "", errChatAgentdUnavailable
	}

	mode := agentd.ModeAutonomous
	if !autonomous {
		// autonomous=false means Guided, never a wider mode. Without a Guided
		// gate, fall back rather than widen every existing chat link.
		if !ladderOffers(st.Autonomy.Available, agentd.ModeGuided) {
			return "", errChatAgentdUnavailable
		}
		mode = agentd.ModeGuided
	}

	turn, err := c.Submit(ctx, agentd.SubmitRequest{
		Message: msg, Mode: mode, Source: "chat", Thread: "latest",
	})
	if err != nil {
		// Only unreachability falls back; retrying a spend-cap or queue
		// refusal on the old path would bypass the limit.
		if errors.Is(err, agentd.ErrAgentUnreachable) {
			return "", errChatAgentdUnavailable
		}
		return "", err
	}

	return s.waitForChatTurn(ctx, c, turn.ID)
}

// waitForChatTurn polls until the turn ends, and returns its reply.
func (s *Server) waitForChatTurn(ctx context.Context, c *agentd.Client, turnID string) (string, error) {
	poll := time.NewTicker(chatPollInterval)
	defer poll.Stop()
	deadline := time.Now().Add(chatTurnTimeout)
	var awaitingSince time.Time
	for {
		select {
		case <-ctx.Done():
			// The turn keeps running; its reply still lands in the transcript.
			return "", ctx.Err()
		case <-poll.C:
		}

		t, err := c.Turn(ctx, turnID)
		if err != nil {
			return "", err
		}
		if t.State == agentd.TurnAwaiting {
			if awaitingSince.IsZero() {
				awaitingSince = time.Now()
			} else if time.Since(awaitingSince) > awaitingGrace {
				return s.parkedReply(ctx, c, t), nil
			}
		} else {
			// Reset so a later park in the same turn gets its own grace.
			awaitingSince = time.Time{}
		}

		switch t.State {
		case agentd.TurnDone:
			return t.Response, nil
		case agentd.TurnFailed:
			if t.Error != "" {
				return "", errors.New(t.Error)
			}
			return "", errors.New("the agent did not finish successfully")
		case agentd.TurnInterrupted:
			// Return partial output so an interrupted chat shows how far it got.
			if t.Response != "" {
				return t.Response, nil
			}
			return "", errors.New("the turn was interrupted")
		}

		if time.Now().After(deadline) {
			return "", errors.New("the agent is still working; this blocking endpoint gave up " +
				"waiting but the turn is still queued and its reply will appear in the " +
				"transcript. Watch /agent/events instead of blocking")
		}
	}
}

// parkedReply names what the agent is waiting on; a vaguer sentence if the
// approvals list is unreadable.
func (s *Server) parkedReply(ctx context.Context, c *agentd.Client, t *agentd.Turn) string {
	what := ""
	if list, err := c.Approvals(ctx); err == nil {
		if pending, ok := list["approvals"].([]any); ok {
			for _, raw := range pending {
				p, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				// This turn's approvals only; the queue may hold another turn's park.
				if id, _ := p["turn_id"].(string); id != "" && id != t.ID {
					continue
				}
				if sig, _ := p["signature"].(string); sig != "" {
					what = sig
					break
				}
				if tool, _ := p["tool"].(string); tool != "" {
					what = tool
					break
				}
			}
		}
	}
	if what == "" {
		return "I have paused for approval before doing something outside what is allowed " +
			"automatically. Approve or deny it on the sandbox's Agent tab and I will carry on — " +
			"the task is still queued."
	}
	return "I need approval to run `" + what + "` before I can carry on. Approve or deny it on " +
		"the sandbox's Agent tab; the task stays queued until you do, and I will continue from " +
		"there."
}

func ladderOffers(available []string, mode string) bool {
	return slices.Contains(available, mode)
}
