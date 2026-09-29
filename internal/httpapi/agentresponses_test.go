//go:build !core

package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
)

func TestResponseStatusMapping(t *testing.T) {
	for state, want := range map[agentd.TurnState]string{
		agentd.TurnQueued:      "queued",
		agentd.TurnRunning:     "in_progress",
		agentd.TurnAwaiting:    "in_progress",
		agentd.TurnDone:        "completed",
		agentd.TurnFailed:      "failed",
		agentd.TurnInterrupted: "cancelled",
	} {
		if got := responseStatusFor(state); got != want {
			t.Errorf("state %q maps to %q, want %q", state, got, want)
		}
	}
	// An unknown state must not claim to be finished, or a client stops polling.
	if got := responseStatusFor(agentd.TurnState("something_new")); got != "in_progress" {
		t.Errorf("unknown state maps to %q, want in_progress", got)
	}
}

func TestToResponseShape(t *testing.T) {
	code := 0
	turn := &agentd.Turn{
		ID: "turn_1", State: agentd.TurnDone, SessionID: "sess_9",
		Response: "all done", ExitCode: &code,
	}
	got := toResponse("sb_1", turn)

	if got.Object != "response" {
		t.Errorf("object = %q", got.Object)
	}
	if got.ID != "turn_1" || got.Status != "completed" {
		t.Errorf("id/status = %q/%q", got.ID, got.Status)
	}
	if got.SessionID != "sess_9" || got.OutputText != "all done" {
		t.Errorf("session/output = %q/%q", got.SessionID, got.OutputText)
	}
	if got.Sandbox != "sb_1" {
		t.Errorf("sandbox = %q", got.Sandbox)
	}
	if got.Native == nil {
		t.Error("the native turn was dropped, making this surface strictly worse than the one it wraps")
	}
	if got.Error != nil {
		t.Errorf("a completed turn carries an error: %+v", got.Error)
	}
}

func TestToResponseAlwaysExplainsAFailure(t *testing.T) {
	code := 3
	got := toResponse("sb_1", &agentd.Turn{ID: "t", State: agentd.TurnFailed, ExitCode: &code})
	if got.Error == nil {
		t.Fatal("a failed turn carries no error object")
	}
	if got.Error.Message == "" {
		t.Error("failure message is empty")
	}

	// An explicit error wins over the exit code.
	got = toResponse("sb_1", &agentd.Turn{
		ID: "t", State: agentd.TurnFailed, ExitCode: &code, Error: "credit balance too low",
	})
	if got.Error.Message != "credit balance too low" {
		t.Errorf("message = %q, want the agent's own explanation", got.Error.Message)
	}

	got = toResponse("sb_1", &agentd.Turn{ID: "t", State: agentd.TurnFailed})
	if got.Error == nil || got.Error.Message == "" {
		t.Error("a failure with no detail still needs a message")
	}
}

func TestEventTranslationCoversTheResponsesVocabulary(t *testing.T) {
	cases := []struct {
		journalType string
		payload     string
		wantName    string
		wantEnd     bool
	}{
		{agentd.EventTurnStarted, `{}`, "response.in_progress", false},
		{agentd.EventTurnThinking, `{"text":"let me look"}`, "response.reasoning.delta", false},
		{agentd.EventTurnOutput, `{"text":"hello"}`, "response.output_text.delta", false},
		{agentd.EventToolStarted, `{"tool":"Bash","tool_id":"t1","detail":"npm test"}`, "response.tool_call.started", false},
		{agentd.EventToolFinished, `{"tool":"Bash","tool_id":"t1","ms":340}`, "response.tool_call.completed", false},
		{agentd.EventToolFailed, `{"tool":"Bash","tool_id":"t1","result":"exit 1"}`, "response.tool_call.failed", false},
		{agentd.EventTurnFinished, `{"exit_code":0}`, "response.completed", true},
		{agentd.EventTurnFailed, `{"exit_code":1}`, "response.failed", true},
		{agentd.EventTurnInterrupted, `{}`, "response.cancelled", true},
	}
	for _, c := range cases {
		name, payload, terminal := translateEvent(c.journalType, json.RawMessage(c.payload))
		if name != c.wantName {
			t.Errorf("%s -> %q, want %q", c.journalType, name, c.wantName)
		}
		if terminal != c.wantEnd {
			t.Errorf("%s terminal = %v, want %v", c.journalType, terminal, c.wantEnd)
		}
		if payload == nil {
			t.Errorf("%s produced a nil payload", c.journalType)
		}
	}
}

func TestEventsWithNoCounterpartAreSkipped(t *testing.T) {
	for _, typ := range []string{
		agentd.EventSessionStarting,
		agentd.EventSessionReady,
		agentd.EventSessionStopped,
		agentd.EventSessionConfig,
		agentd.EventTurnQueued,
		"some.future.event",
	} {
		if name, _, _ := translateEvent(typ, json.RawMessage(`{}`)); name != "" {
			t.Errorf("%s translated to %q, want skipped", typ, name)
		}
	}
}

func TestTruncationIsAnnouncedNotHidden(t *testing.T) {
	name, _, terminal := translateEvent(agentd.EventTurnOutputTruncated,
		json.RawMessage(`{"reason":"per-turn output limit reached"}`))
	if name != "response.output_text.truncated" {
		t.Errorf("name = %q", name)
	}
	if terminal {
		t.Error("truncation ended the stream; the turn is still running")
	}
}

func TestApprovalIsANonStandardEvent(t *testing.T) {
	name, payload, _ := translateEvent(agentd.EventApprovalRequested,
		json.RawMessage(`{"approval_id":"apr_1","tool":"Bash"}`))
	if name != "response.approval.requested" {
		t.Errorf("name = %q", name)
	}
	m, _ := payload.(map[string]any)
	if m["approval_id"] != "apr_1" {
		t.Errorf("approval payload lost its id: %v", payload)
	}
}

func TestUsageIsOmittedWhenNotReported(t *testing.T) {
	_, payload, _ := translateEvent(agentd.EventTurnFinished, json.RawMessage(`{"exit_code":0}`))
	m, _ := payload.(map[string]any)
	if _, present := m["usage"]; present {
		t.Errorf("usage appeared with nothing reported: %v", m)
	}

	_, payload, _ = translateEvent(agentd.EventTurnFinished,
		json.RawMessage(`{"exit_code":0,"input_tokens":100,"output_tokens":20,"model_cost_usd":0.01}`))
	m, _ = payload.(map[string]any)
	u, ok := m["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %v", m)
	}
	if u["input_tokens"] != float64(100) || u["output_tokens"] != float64(20) {
		t.Errorf("tokens = %v", u)
	}
	// total_tokens is derived.
	if u["total_tokens"] != float64(120) {
		t.Errorf("total_tokens = %v, want 120", u["total_tokens"])
	}
	if u["cost_usd"] != 0.01 {
		t.Errorf("cost = %v", u["cost_usd"])
	}
}

func TestResponsesSurfaceIsBehindTheAgentGate(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "base") // base is kind=shell
	base := "/v1/sandboxes/" + id + "/agent/responses"

	for _, c := range []struct{ method, path, body string }{
		{"POST", base, `{"input":"hello"}`},
		{"GET", base + "/turn_x", ""},
		{"GET", base + "/turn_x/stream", ""},
		{"POST", base + "/turn_x/cancel", `{}`},
	} {
		code, out := do(t, c.method, srv+c.path, key, c.body)
		if code != http.StatusNotFound {
			t.Errorf("%s %s on a non-agent sandbox = %d, want 404", c.method, c.path, code)
		}
		if msg, _ := out["error"].(string); msg != "sandbox not found" {
			t.Errorf("%s %s leaked %q", c.method, c.path, msg)
		}
	}
}

func TestResponsesRequireAuthAndAnInput(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	base := "/v1/sandboxes/" + id + "/agent/responses"

	if code, _ := do(t, "POST", srv+base, "", `{"input":"hi"}`); code != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", code)
	}
	// message is accepted as an alias of input.
	for _, body := range []string{`{}`, `{"input":""}`, `{"input":"   "}`, `{"message":""}`} {
		if code, _ := do(t, "POST", srv+base, key, body); code != http.StatusBadRequest {
			t.Errorf("input %s = %d, want 400", body, code)
		}
	}
}

// The surface stays sandbox-scoped; a root-level path would read as an inference API.
func TestThereIsNoRootLevelResponsesEndpoint(t *testing.T) {
	srv, key, _ := newAgentTestAPI(t, "claude")
	code, _ := do(t, "POST", srv+"/v1/responses", key, `{"input":"hi"}`)
	if code != http.StatusNotFound {
		t.Errorf("POST /v1/responses = %d, want 404 — this must stay sandbox-scoped", code)
	}
}
