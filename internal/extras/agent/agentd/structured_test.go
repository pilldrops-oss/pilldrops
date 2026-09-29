package agentd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd/parse"
)

// newStructuredRunner runs a script whose stdout is the agent's JSONL event stream.
func newStructuredRunner(t *testing.T, script string) (*Runner, *Journal) {
	t.Helper()
	j := openTestJournal(t, t.TempDir())
	r := &Runner{
		Journal: j,
		Build:   func(*Turn) (string, []string, error) { return script, nil, nil },
		Grace:   200 * time.Millisecond,
		Parser:  parse.For(parse.FormatClaude),
		// Every turn is structured in these tests.
		Structured: func(*Turn) bool { return true },
	}
	return r, j
}

func eventsByType(t *testing.T, j *Journal) map[string][]Event {
	t.Helper()
	out := map[string][]Event{}
	for _, ev := range collect(t, j, 0) {
		out[ev.Type] = append(out[ev.Type], ev)
	}
	return out
}

func payload(t *testing.T, ev Event) map[string]any {
	t.Helper()
	m := map[string]any{}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatalf("payload: %v", err)
		}
	}
	return m
}

func TestStructuredStreamBecomesAToolTimeline(t *testing.T) {
	frames := []string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-8","tools":["Bash"],"claude_code_version":"2.1.197"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"npm test"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"2 passing"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Tests pass."}]}}`,
		`{"type":"result","subtype":"success","result":"All done.","total_cost_usd":0.0731,"usage":{"input_tokens":1200,"output_tokens":95}}`,
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("printf '%s\\n' " + shellSingleQuote(f) + "; ")
	}
	r, j := newStructuredRunner(t, sb.String())

	res, err := r.Run(context.Background(), turnFor("run the tests"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	byType := eventsByType(t, j)

	if len(byType[EventSessionConfig]) != 1 {
		t.Fatalf("session config events = %d, want 1", len(byType[EventSessionConfig]))
	}
	cfg := payload(t, byType[EventSessionConfig][0])
	if cfg["model"] != "claude-opus-4-8" || cfg["session_id"] != "s1" {
		t.Errorf("session config = %v", cfg)
	}

	if len(byType[EventToolStarted]) != 1 {
		t.Fatalf("tool.started = %d, want 1", len(byType[EventToolStarted]))
	}
	start := payload(t, byType[EventToolStarted][0])
	if start["tool"] != "Bash" || start["detail"] != "npm test" {
		t.Errorf("tool start = %v", start)
	}

	if len(byType[EventToolFinished]) != 1 {
		t.Fatalf("tool.finished = %d, want 1", len(byType[EventToolFinished]))
	}
	fin := payload(t, byType[EventToolFinished][0])
	if fin["tool_id"] != "t1" {
		t.Errorf("tool finish did not correlate by id: %v", fin)
	}
	if _, ok := fin["ms"]; !ok {
		t.Error("no duration on the finished tool; the start/end correlation failed")
	}

	if len(byType[EventTurnFinished]) != 1 {
		t.Fatalf("turn.finished = %d", len(byType[EventTurnFinished]))
	}
	done := payload(t, byType[EventTurnFinished][0])
	if done["model_cost_usd"] != 0.0731 {
		t.Errorf("cost = %v, want 0.0731", done["model_cost_usd"])
	}
	if done["input_tokens"] != float64(1200) {
		t.Errorf("input tokens = %v", done["input_tokens"])
	}
	if done["session_id"] != "s1" {
		t.Errorf("session id not carried to the terminal event: %v", done)
	}

	if strings.Contains(res.Reply, `"type"`) {
		t.Errorf("reply leaked raw JSON to the chat path: %q", res.Reply)
	}
	if !strings.Contains(res.Reply, "Tests pass.") || !strings.Contains(res.Reply, "All done.") {
		t.Errorf("reply = %q, want the agent's text", res.Reply)
	}
}

func TestStructuredStepsAreNumberedInOrder(t *testing.T) {
	frames := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Read"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b","name":"Edit"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b"}]}}`,
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("printf '%s\\n' " + shellSingleQuote(f) + "; ")
	}
	r, j := newStructuredRunner(t, sb.String())
	if _, err := r.Run(context.Background(), turnFor("two tools")); err != nil {
		t.Fatal(err)
	}

	var steps []int
	for _, ev := range collect(t, j, 0) {
		if ev.Type == EventToolStarted || ev.Type == EventToolFinished {
			steps = append(steps, ev.Step)
		}
	}
	if len(steps) != 4 {
		t.Fatalf("got %d tool events, want 4", len(steps))
	}
	for i := 1; i < len(steps); i++ {
		if steps[i] <= steps[i-1] {
			t.Errorf("step numbers not increasing: %v", steps)
			break
		}
	}
}

func TestInterleavedToolCallsCorrelateByID(t *testing.T) {
	frames := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"slow","name":"Build"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"fast","name":"Read"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"fast"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"slow"}]}}`,
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("printf '%s\\n' " + shellSingleQuote(f) + "; ")
	}
	r, j := newStructuredRunner(t, sb.String())
	if _, err := r.Run(context.Background(), turnFor("interleaved")); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, ev := range eventsByType(t, j)[EventToolFinished] {
		p := payload(t, ev)
		id, _ := p["tool_id"].(string)
		seen[id] = true
		if _, ok := p["ms"]; !ok {
			t.Errorf("tool %q finished with no duration; its start was not matched", id)
		}
	}
	for _, want := range []string{"slow", "fast"} {
		if !seen[want] {
			t.Errorf("tool %q never finished", want)
		}
	}
}

func TestUnrecognisedFramesStillReachTheUserAsOutput(t *testing.T) {
	r, j := newStructuredRunner(t,
		`printf '%s\n' '{"type":"future.frame","note":"nobody knows this yet"}'; `+
			`printf '%s\n' 'a plain log line'`)
	if _, err := r.Run(context.Background(), turnFor("unknown frames")); err != nil {
		t.Fatal(err)
	}

	out := outputOf(t, j, "")
	if !strings.Contains(out, "future.frame") {
		t.Error("an unrecognised JSON frame was dropped instead of shown")
	}
	if !strings.Contains(out, "a plain log line") {
		t.Error("a non-JSON line was dropped instead of shown")
	}
}

func TestFailedToolIsJournaledAsAFailure(t *testing.T) {
	frames := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"false"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"x","is_error":true,"content":"exit 1"}]}}`,
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("printf '%s\\n' " + shellSingleQuote(f) + "; ")
	}
	r, j := newStructuredRunner(t, sb.String())
	if _, err := r.Run(context.Background(), turnFor("failing tool")); err != nil {
		t.Fatal(err)
	}

	byType := eventsByType(t, j)
	if len(byType[EventToolFailed]) != 1 {
		t.Fatalf("tool.failed = %d, want 1", len(byType[EventToolFailed]))
	}
	if len(byType[EventToolFinished]) != 0 {
		t.Error("a failed tool was also journaled as finished")
	}
	if p := payload(t, byType[EventToolFailed][0]); !strings.Contains(p["result"].(string), "exit 1") {
		t.Errorf("failure reason lost: %v", p)
	}
}

func TestNoCostReportedMeansNoCostField(t *testing.T) {
	r, j := newStructuredRunner(t,
		`printf '%s\n' '{"type":"result","subtype":"success","result":"done"}'`)
	if _, err := r.Run(context.Background(), turnFor("no cost")); err != nil {
		t.Fatal(err)
	}
	done := eventsByType(t, j)[EventTurnFinished]
	if len(done) != 1 {
		t.Fatalf("turn.finished = %d", len(done))
	}
	if _, present := payload(t, done[0])["model_cost_usd"]; present {
		t.Error("a cost field appeared when the agent reported none")
	}
}

func TestWithoutAParserTheStreamIsPlainOutput(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	frame := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Bash"}]}}`
	r := &Runner{
		Journal: j,
		Build:   func(*Turn) (string, []string, error) { return "printf '%s\\n' " + shellSingleQuote(frame), nil, nil },
		Grace:   200 * time.Millisecond,
		// No Parser, no Structured.
	}
	if _, err := r.Run(context.Background(), turnFor("plain")); err != nil {
		t.Fatal(err)
	}
	byType := eventsByType(t, j)
	if len(byType[EventToolStarted]) != 0 {
		t.Error("a tool event was journaled without a parser configured")
	}
	if !strings.Contains(outputOf(t, j, "stdout"), "tool_use") {
		t.Error("the raw frame was not journaled as output")
	}
}

func TestStructuredIsDecidedPerTurnNotPerSession(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	r := &Runner{
		Journal:    j,
		Build:      func(*Turn) (string, []string, error) { return `printf 'plain prose reply'`, nil, nil },
		Grace:      200 * time.Millisecond,
		Parser:     parse.For(parse.FormatClaude),
		Structured: func(t *Turn) bool { return !t.FollowUp },
	}
	turn := turnFor("continue please")
	turn.FollowUp = true

	if _, err := r.Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if got := outputOf(t, j, "stdout"); got != "plain prose reply" {
		t.Errorf("stdout = %q, want the prose verbatim", got)
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
