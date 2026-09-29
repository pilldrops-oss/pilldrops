package parse

import (
	"os"
	"strings"
	"testing"
)

// testdata/claude-live-turn.jsonl is real stdout from "claude -p --output-format
// stream-json --verbose", edited only to rewrite the scratch path to /work.
func TestClaudeParserAgainstALiveCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/claude-live-turn.jsonl")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	lines := splitNonEmpty(string(data))
	if len(lines) != 6 {
		t.Fatalf("capture has %d frames, expected the 6 that were recorded", len(lines))
	}

	p := claudeParser{}
	var steps []Step
	for _, ln := range lines {
		steps = append(steps, p.Parse([]byte(ln)))
	}

	// The order matters: the tool's completion arrives in a user frame, not an assistant one.
	wantKinds := []Kind{
		KindSession,   // system/init
		KindThinking,  // assistant, thinking block (empty text here)
		KindToolStart, // assistant, tool_use block
		KindToolEnd,   // user, tool_result block
		KindText,      // assistant, text block
		KindResult,    // result
	}
	for i, want := range wantKinds {
		if steps[i].Kind != want {
			t.Fatalf("frame %d: kind %q, want %q\nraw: %s",
				i, steps[i].Kind, want, truncFor(lines[i]))
		}
		if len(steps[i].Raw) == 0 {
			t.Fatalf("frame %d: Raw not preserved", i)
		}
	}

	for i, s := range steps {
		if s.Kind == KindUnknown {
			t.Fatalf("frame %d fell through to unknown: %s", i, truncFor(lines[i]))
		}
	}
}

func TestLiveCaptureSessionFrameCarriesTheSessionSurface(t *testing.T) {
	steps := parseLiveCapture(t)
	init := steps[0]

	if init.SessionID == "" {
		t.Fatal("no session id: --resume cannot address this conversation")
	}
	if init.Model == "" {
		t.Fatal("no model: the UI cannot say what answered")
	}
	if len(init.Tools) == 0 {
		t.Fatal("no tool list on the init frame")
	}
	if init.PermissionMode == "" {
		t.Fatal("no permissionMode: the UI cannot state how autonomous this run is")
	}
}

func TestLiveCaptureToolCallCorrelatesStartToEnd(t *testing.T) {
	steps := parseLiveCapture(t)

	var start, end Step
	for _, s := range steps {
		switch s.Kind {
		case KindToolStart:
			start = s
		case KindToolEnd:
			end = s
		}
	}
	if start.Tool != "Write" {
		t.Fatalf("tool name = %q, want Write", start.Tool)
	}
	if start.ToolID == "" {
		t.Fatal("tool_use carried no id")
	}
	if end.ToolID != start.ToolID {
		t.Fatalf("tool_result id %q does not match tool_use id %q — the timeline "+
			"cannot pair them", end.ToolID, start.ToolID)
	}
	// The real block uses "file_path", not "path".
	if start.Detail == "" {
		t.Fatal("no detail on the tool call: the timeline row would name no target")
	}
	if !strings.Contains(start.Detail, "hello.txt") {
		t.Fatalf("detail does not name the file acted on: %q", start.Detail)
	}
}

func TestLiveCaptureResultCarriesRealCost(t *testing.T) {
	steps := parseLiveCapture(t)
	res := steps[len(steps)-1]

	if res.Kind != KindResult {
		t.Fatalf("last frame is %q, not a result", res.Kind)
	}
	if res.Text == "" {
		t.Fatal("result frame carried no final answer")
	}
	// The manifest claims CostSource "stream"; without this the spend cap measures nothing.
	if res.Usage.CostUSD == nil {
		t.Fatal("no total_cost_usd: the spend cap would count every claude turn as unreported")
	}
	if *res.Usage.CostUSD <= 0 {
		t.Fatalf("cost reported as %v, which the budget treats as no spend", *res.Usage.CostUSD)
	}
}

func parseLiveCapture(t *testing.T) []Step {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-live-turn.jsonl")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	p := claudeParser{}
	var steps []Step
	for _, ln := range splitNonEmpty(string(data)) {
		steps = append(steps, p.Parse([]byte(ln)))
	}
	return steps
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func truncFor(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// testdata/claude-live-followup.jsonl is the second turn of a conversation, captured
// from "claude -p --continue --output-format stream-json --verbose". It verifies
// the StreamContinueCmd claim in the manifest.
func TestClaudeFollowUpTurnIsFullyStructured(t *testing.T) {
	data, err := os.ReadFile("testdata/claude-live-followup.jsonl")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	p := claudeParser{}
	var kinds []Kind
	var steps []Step
	for _, ln := range splitNonEmpty(string(data)) {
		s := p.Parse([]byte(ln))
		steps = append(steps, s)
		kinds = append(kinds, s.Kind)
	}

	for _, need := range []Kind{KindSession, KindToolStart, KindToolEnd, KindText, KindResult} {
		found := false
		for _, k := range kinds {
			if k == need {
				found = true
			}
		}
		if !found {
			t.Fatalf("a --continue turn produced no %s frame; got %v", need, kinds)
		}
	}
	for i, s := range steps {
		if s.Kind == KindUnknown {
			t.Fatalf("follow-up frame %d fell through to unknown", i)
		}
	}

	last := steps[len(steps)-1]
	if last.Usage.CostUSD == nil || *last.Usage.CostUSD <= 0 {
		t.Fatalf("follow-up reported no cost: %+v", last.Usage)
	}

	if steps[0].SessionID == "" {
		t.Fatal("follow-up init frame carried no session id")
	}
}

// The two fixtures are halves of one conversation; a fresh conversation would
// also exit 0 with a clean stream, so the session id is what tells them apart.
func TestClaudeContinueKeepsTheSameSession(t *testing.T) {
	p := claudeParser{}
	sessionOf := func(file string) string {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, ln := range splitNonEmpty(string(data)) {
			if s := p.Parse([]byte(ln)); s.Kind == KindSession {
				return s.SessionID
			}
		}
		t.Fatalf("%s has no session frame", file)
		return ""
	}
	first := sessionOf("testdata/claude-live-first.jsonl")
	second := sessionOf("testdata/claude-live-followup.jsonl")
	if first == "" {
		t.Fatal("opening turn carried no session id")
	}
	if first != second {
		t.Fatalf("--continue started a NEW conversation: %s -> %s; a follow-up turn "+
			"would silently forget everything", first, second)
	}
}

// testdata/claude-live-denied.jsonl is a live turn whose Write was refused by a
// stdio MCP approver wired in with --permission-prompt-tool; the file was not written.
func TestClaudeDeniedTurnCarriesItsDenials(t *testing.T) {
	data, err := os.ReadFile("testdata/claude-live-denied.jsonl")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	p := claudeParser{}
	var result Step
	for _, ln := range splitNonEmpty(string(data)) {
		if s := p.Parse([]byte(ln)); s.Kind == KindResult {
			result = s
		}
	}
	if result.Kind != KindResult {
		t.Fatal("no result frame in the denied capture")
	}
	if len(result.Denials) != 1 {
		t.Fatalf("denials = %d, want 1 — a turn whose actions were all refused would "+
			"otherwise look like a turn that chose to do nothing", len(result.Denials))
	}
	d := result.Denials[0]
	if d.Tool != "Write" {
		t.Fatalf("denied tool = %q, want Write", d.Tool)
	}
	if d.ToolID == "" {
		t.Fatal("denial carries no tool id, so it cannot be tied to its timeline row")
	}
	if !strings.Contains(d.Detail, "denied.txt") {
		t.Fatalf("denial detail does not name the target: %q", d.Detail)
	}
}

func TestUndeniedTurnHasNoDenials(t *testing.T) {
	for _, s := range parseLiveCapture(t) {
		if len(s.Denials) != 0 {
			t.Fatalf("%s frame reported denials on a successful turn: %+v", s.Kind, s.Denials)
		}
	}
}
