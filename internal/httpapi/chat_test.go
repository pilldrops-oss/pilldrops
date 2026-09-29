//go:build !core

package httpapi

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/bridge/telegram"
)

func TestChatRequiresOwnership(t *testing.T) {
	srv, _, sb, st := newCronSandbox(t)

	_, otherKey, err := st.CreateKey(t.Context(), "other", "org_other")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/chat", otherKey, `{"message":"hello"}`); code != http.StatusNotFound {
		t.Errorf("cross-tenant chat = %d, want 404", code)
	}
	if code, _ := do(t, "POST", srv+"/v1/sandboxes/sb_nope/chat", otherKey, `{"message":"hi"}`); code != http.StatusNotFound {
		t.Errorf("unknown sandbox = %d, want 404", code)
	}
}

func TestChatRejectsEmptyMessage(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)
	for _, body := range []string{`{"message":""}`, `{"message":"   "}`, `{}`} {
		if code, _ := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/chat", key, body); code != http.StatusBadRequest {
			t.Errorf("empty message %s did not 400", body)
		}
	}
}

func TestChatOnANonAgentTemplateExplainsItself(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/chat", key, `{"message":"what can you do?"}`)
	if code != http.StatusBadRequest && code != http.StatusOK {
		t.Fatalf("chat = %d %v, want a clear 400 or a reply", code, out)
	}
	if code == http.StatusBadRequest {
		msg, _ := out["error"].(string)
		if !strings.Contains(strings.ToLower(msg), "agent") {
			t.Errorf("refusal %q does not mention the missing agent", msg)
		}
	}
}

func TestChatHistoryIsReturnedAndScoped(t *testing.T) {
	srv, key, sb, st := newCronSandbox(t)

	code, out := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/chat", key, "")
	if code != http.StatusOK {
		t.Fatalf("history = %d %v", code, out)
	}
	if _, ok := out["messages"]; !ok {
		t.Error("history has no messages field")
	}

	_, otherKey, _ := st.CreateKey(t.Context(), "other2", "org_other2")
	if code, _ := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/chat", otherKey, ""); code != http.StatusNotFound {
		t.Errorf("cross-tenant history = %d, want 404", code)
	}
}

// Both turns are recorded even when the agent errored.
func TestChatRecordsBothTurns(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/chat", key, `{"message":"first question"}`)

	_, out := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/chat", key, "")
	msgs, _ := out["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("transcript has %d messages, want the user turn and a reply", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || !strings.Contains(first["text"].(string), "first question") {
		t.Errorf("first message = %v, want the user's own text", first)
	}
	if msgs[1].(map[string]any)["role"] != "agent" {
		t.Errorf("second message role = %v, want agent", msgs[1])
	}
}

// Agent CLIs keep their transcript on disk, so concurrent turns corrupt the agent's memory.
func TestChatRefusesAConcurrentTurn(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	release, ok := telegram.AcquireTurn(sb)
	if !ok {
		t.Fatal("could not take the turn slot")
	}
	defer release()

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/chat", key, `{"message":"second message"}`)
	if code != http.StatusConflict {
		t.Fatalf("concurrent chat = %d, want 409", code)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(strings.ToLower(msg), "still working") {
		t.Errorf("busy message %q does not explain the wait", msg)
	}

	release()
	if code, _ := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/chat", key, `{"message":"retry"}`); code == http.StatusConflict {
		t.Error("still 409 after the slot was released — the workstation is wedged")
	}
}

// Mapping autonomous=false to ModeAutonomous would widen every chat link to
// full write access. Pinned at the source because the behavioural path needs a live agentd.
func TestChatMapsNonAutonomousToGuidedNotToAutonomous(t *testing.T) {
	src, err := os.ReadFile("chatagentd.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "mode = agentd.ModeGuided") {
		t.Error("the non-autonomous branch does not select Guided, so a read-only chat link " +
			"would be widened to whatever the default mode is")
	}
	// When Guided is unavailable it must fall back to the old path, not a wider mode.
	if !strings.Contains(body, "if !ladderOffers(st.Autonomy.Available, agentd.ModeGuided)") {
		t.Error("Guided availability is not checked before using it")
	}
	if !strings.Contains(body, "return \"\", errChatAgentdUnavailable") {
		t.Error("an agent without a gate does not fall back, so a non-autonomous request " +
			"would be served in a wider mode")
	}
}

// Retrying a spend-cap refusal on the pre-agentd path would bypass the cap.
func TestChatRefusalIsNotRetriedOnTheOldPath(t *testing.T) {
	src, err := os.ReadFile("chatagentd.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "if errors.Is(err, agentd.ErrAgentUnreachable)") {
		t.Fatal("submit failures are not distinguished, so a spend-cap refusal could be " +
			"retried on a path that does not enforce the cap")
	}
}

func TestChatDeprecationCarriesADate(t *testing.T) {
	if chatSunsetDate == "" {
		t.Fatal("the blocking contract is deprecated with no sunset date")
	}
	when, err := time.Parse("2006-01-02", chatSunsetDate)
	if err != nil {
		t.Fatalf("sunset date %q is not a date: %v", chatSunsetDate, err)
	}
	if when.Before(time.Now().Add(60 * 24 * time.Hour)) {
		t.Errorf("sunset %s leaves less than two months to migrate an integration", chatSunsetDate)
	}
}
