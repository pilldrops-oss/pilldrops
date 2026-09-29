package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// recordingBackend captures what a tool asked the platform API for.
type recordingBackend struct {
	calls  []beCall
	status int
	body   string
	err    error
}

type beCall struct{ method, path, body, ctype string }

func (b *recordingBackend) fn(method, path string, body []byte, ctype string) (int, []byte, error) {
	b.calls = append(b.calls, beCall{method, path, string(body), ctype})
	if b.err != nil {
		return 0, nil, b.err
	}
	st := b.status
	if st == 0 {
		st = 200
	}
	body2 := b.body
	if body2 == "" {
		body2 = `{"ok":true}`
	}
	return st, []byte(body2), nil
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

func rpcErrorOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	m := decode(t, raw)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("want an error response, got %s", raw)
	}
	return e
}

func TestNotificationsGetNoResponse(t *testing.T) {
	be := &recordingBackend{}
	if got := HandleMessage([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), be.fn); got != nil {
		t.Errorf("notification answered with %s, want no reply", got)
	}
}

func TestBatchOfOnlyNotificationsReturnsNothing(t *testing.T) {
	be := &recordingBackend{}
	raw := `[{"jsonrpc":"2.0","method":"notifications/initialized"},
	         {"jsonrpc":"2.0","method":"notifications/cancelled"}]`
	if got := HandleMessage([]byte(raw), be.fn); got != nil {
		t.Errorf("all-notification batch answered with %s, want no reply", got)
	}
}

func TestBatchDropsNotificationsButAnswersRequests(t *testing.T) {
	be := &recordingBackend{}
	raw := `[{"jsonrpc":"2.0","method":"notifications/initialized"},
	         {"jsonrpc":"2.0","id":7,"method":"ping"}]`
	got := HandleMessage([]byte(raw), be.fn)

	var arr []map[string]any
	if err := json.Unmarshal(got, &arr); err != nil {
		t.Fatalf("batch reply is not an array: %v (%s)", err, got)
	}
	if len(arr) != 1 {
		t.Fatalf("got %d replies, want 1 — the notification must not be answered", len(arr))
	}
	if arr[0]["id"] != float64(7) {
		t.Errorf("reply id = %v, want 7", arr[0]["id"])
	}
}

func TestIDsSurviveTheirOriginalType(t *testing.T) {
	be := &recordingBackend{}
	for _, tc := range []struct{ id, want string }{
		{`7`, `7`},
		{`"abc"`, `"abc"`},
		{`"7"`, `"7"`},
	} {
		got := HandleMessage([]byte(`{"jsonrpc":"2.0","id":`+tc.id+`,"method":"ping"}`), be.fn)
		if !strings.Contains(string(got), `"id":`+tc.want) {
			t.Errorf("id %s came back as %s, want %s preserved verbatim", tc.id, got, tc.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	be := &recordingBackend{}
	for name, raw := range map[string]string{
		"truncated object": `{"jsonrpc":"2.0"`,
		"not json":         `hello`,
		"truncated array":  `[{"jsonrpc":"2.0"}`,
	} {
		e := rpcErrorOf(t, HandleMessage([]byte(raw), be.fn))
		if e["code"] != float64(-32700) {
			t.Errorf("%s: code = %v, want -32700 (parse error)", name, e["code"])
		}
	}
	if len(be.calls) != 0 {
		t.Errorf("a malformed message reached the backend: %v", be.calls)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	be := &recordingBackend{}
	e := rpcErrorOf(t, HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`), be.fn))
	if e["code"] != float64(-32601) {
		t.Errorf("code = %v, want -32601", e["code"])
	}
}

func TestInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	be := &recordingBackend{}
	for _, tc := range []struct{ ask, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-03-26"},
		// Unsupported versions get the newest rather than an error.
		{"1999-01-01", protocolVersions[0]},
		{"", protocolVersions[0]},
	} {
		raw := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + tc.ask + `"}}`
		res := decode(t, HandleMessage([]byte(raw), be.fn))["result"].(map[string]any)
		if got := res["protocolVersion"]; got != tc.want {
			t.Errorf("initialize(%q) negotiated %v, want %v", tc.ask, got, tc.want)
		}
	}
}

func TestInitializeAdvertisesTools(t *testing.T) {
	be := &recordingBackend{}
	res := decode(t, HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`), be.fn))["result"].(map[string]any)
	caps, ok := res["capabilities"].(map[string]any)
	if !ok || caps["tools"] == nil {
		t.Errorf("initialize must advertise the tools capability, got %v", res["capabilities"])
	}
}

func TestEveryAdvertisedToolIsDispatchable(t *testing.T) {
	be := &recordingBackend{}
	listed := decode(t, HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), be.fn))
	tools := listed["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("tools/list is empty")
	}

	for _, ent := range tools {
		name := ent.(map[string]any)["name"].(string)
		// timeout_seconds keeps wait_ready short.
		res := callTool(name, map[string]any{"sandbox_id": "sb_test", "timeout_seconds": float64(1)}, be.fn)
		text := resultText(res)
		if strings.Contains(text, "unknown tool") {
			t.Errorf("tools/list advertises %q but callTool has no handler for it", name)
		}
	}
}

func TestEveryToolDeclaresNameDescriptionAndSchema(t *testing.T) {
	be := &recordingBackend{}
	listed := decode(t, HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), be.fn))
	for _, ent := range listed["result"].(map[string]any)["tools"].([]any) {
		d := ent.(map[string]any)
		name, _ := d["name"].(string)
		if name == "" {
			t.Errorf("a tool has no name: %v", d)
			continue
		}
		if desc, _ := d["description"].(string); desc == "" {
			t.Errorf("%s has no description — the model picks tools by description", name)
		}
		if d["inputSchema"] == nil {
			t.Errorf("%s has no inputSchema; a client cannot construct a call", name)
		}
	}
}

func TestUnknownToolIsAnErrorResultNotACrash(t *testing.T) {
	be := &recordingBackend{}
	res := callTool("rm_rf_everything", nil, be.fn)
	if !strings.Contains(resultText(res), "unknown tool") {
		t.Errorf("want an unknown-tool message, got %v", res)
	}
	if len(be.calls) != 0 {
		t.Errorf("an unknown tool reached the backend: %v", be.calls)
	}
}

func resultText(res map[string]any) string {
	content, _ := res["content"].([]map[string]any)
	var sb strings.Builder
	for _, c := range content {
		s, _ := c["text"].(string)
		sb.WriteString(s)
	}
	return sb.String()
}

func TestSandboxIDIsPathEscaped(t *testing.T) {
	be := &recordingBackend{}
	callTool("get_sandbox", map[string]any{"sandbox_id": "../../v1/admin/plans"}, be.fn)
	if len(be.calls) != 1 {
		t.Fatalf("want 1 backend call, got %v", be.calls)
	}
	got := be.calls[0].path
	if strings.Contains(got, "/admin/") {
		t.Errorf("path = %q — a slash in the sandbox id escaped into the route", got)
	}
	if !strings.HasPrefix(got, "/v1/sandboxes/") {
		t.Errorf("path = %q, want it rooted at /v1/sandboxes/", got)
	}
}

func TestAPIErrorsCarryAnActionableHint(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":"unauthorized"}`, "Authorization: Bearer"},
		{404, `{"error":"not found"}`, "list_sandboxes"},
		{402, `{"error":"quota"}`, "get_usage"},
		{403, `{"error":"forbidden"}`, "get_usage"},
		{503, `{"error":"upstream"}`, "retry once"},
	} {
		be := &recordingBackend{status: tc.status, body: tc.body}
		_, errText := apiCall(be.fn, "GET", "/v1/templates", nil, "")
		if !strings.Contains(errText, tc.want) {
			t.Errorf("HTTP %d hint = %q, want it to mention %q", tc.status, errText, tc.want)
		}
	}
}

func TestAPIErrorFallsBackWhenTheBodyHasNoErrorField(t *testing.T) {
	be := &recordingBackend{status: 500, body: `<html>gateway</html>`}
	_, errText := apiCall(be.fn, "GET", "/v1/templates", nil, "")
	if !strings.Contains(errText, "HTTP 500") {
		t.Errorf("errText = %q, want it to state the status when the body is not our JSON", errText)
	}
}

func TestNetworkErrorIsReportedAsSuch(t *testing.T) {
	be := &recordingBackend{err: errDial}
	_, errText := apiCall(be.fn, "GET", "/v1/templates", nil, "")
	if !strings.Contains(errText, "network error") {
		t.Errorf("errText = %q, want it to distinguish a transport failure from an API rejection", errText)
	}
}

var errDial = &net0{}

type net0 struct{}

func (*net0) Error() string { return "dial tcp: connection refused" }

func TestWaitReadyRespectsItsWallClockBudget(t *testing.T) {
	slow := &recordingBackend{body: `{"stdout":"000"}`}
	be := func(method, path string, body []byte, ctype string) (int, []byte, error) {
		time.Sleep(2 * time.Second)
		return slow.fn(method, path, body, ctype)
	}

	start := time.Now()
	res := callTool("wait_ready", map[string]any{"sandbox_id": "sb_x", "timeout_seconds": float64(2)}, be)
	elapsed := time.Since(start)

	// Counting attempts instead of time would take ~6s.
	if elapsed > 4500*time.Millisecond {
		t.Errorf("wait_ready(timeout_seconds=2) took %v — the budget bounds attempts, not time", elapsed)
	}
	if !strings.Contains(resultText(res), `"ready":false`) {
		t.Errorf("want ready:false after the budget expired, got %v", resultText(res))
	}
}

func TestWaitReadyReturnsAsSoonAsTheAppAnswers(t *testing.T) {
	be := &recordingBackend{body: `{"stdout":"200"}`}
	start := time.Now()
	res := callTool("wait_ready", map[string]any{"sandbox_id": "sb_x", "timeout_seconds": float64(30)}, be.fn)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a ready app took %v to report — it must return on the first successful probe", elapsed)
	}
	if !strings.Contains(resultText(res), `"ready":true`) {
		t.Errorf("want ready:true, got %v", resultText(res))
	}
	if len(be.calls) != 1 {
		t.Errorf("probed %d times, want 1", len(be.calls))
	}
}

func TestWaitReadyTreatsServerErrorsAsNotReady(t *testing.T) {
	be := &recordingBackend{body: `{"stdout":"500"}`}
	res := callTool("wait_ready", map[string]any{"sandbox_id": "sb_x", "timeout_seconds": float64(1)}, be.fn)
	if !strings.Contains(resultText(res), `"ready":false`) {
		t.Errorf("HTTP 500 from the app reported as ready: %v", resultText(res))
	}
}

func TestNumAcceptsWhatClientsActuallySend(t *testing.T) {
	for name, tc := range map[string]struct {
		in   any
		want int
	}{
		"json number":     {float64(8080), 8080},
		"stringified":     {"8080", 8080},
		"padded string":   {" 8080 ", 8080},
		"go int":          {8080, 8080},
		"absent":          {nil, 0},
		"not a number":    {"eighty-eighty", 0},
		"bool":            {true, 0},
		"empty string":    {"", 0},
		"float with frac": {float64(80.9), 80},
	} {
		a := map[string]any{}
		if tc.in != nil {
			a["port"] = tc.in
		}
		if got := num(a, "port"); got != tc.want {
			t.Errorf("%s: num(%v) = %d, want %d", name, tc.in, got, tc.want)
		}
	}
}

func TestWaitReadyHonoursAStringifiedPort(t *testing.T) {
	be := &recordingBackend{body: `{"stdout":"200"}`}
	callTool("wait_ready", map[string]any{"sandbox_id": "sb_x", "port": "3000"}, be.fn)
	if len(be.calls) == 0 {
		t.Fatal("no probe issued")
	}
	if !strings.Contains(be.calls[0].body, "127.0.0.1:3000") {
		t.Errorf("probe body = %s, want it to target port 3000, not the 8000 default", be.calls[0].body)
	}
}
