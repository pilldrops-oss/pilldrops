package parse

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnknownLinesPreserveTheirBytesForEveryDialect(t *testing.T) {
	for _, format := range Formats() {
		p := For(format)
		if p == nil {
			t.Fatalf("Formats() lists %q but For() returns nil", format)
		}
		for _, line := range []string{
			`{"type":"some.future.frame","detail":"whatever"}`,
			`{"type":"assistant"}`, // right type, nothing usable inside
			`plain text, not json at all`,
			`2026-08-07T18:17:18Z ERROR codex_api::endpoint: 401 Unauthorized`,
			`{"broken":`,
			``,
			`   `,
			`[1,2,3]`, // valid JSON, wrong shape
		} {
			got := p.Parse([]byte(line))
			if got.Kind != KindUnknown {
				t.Errorf("%s: %q parsed as %s, want unknown", format, line, got.Kind)
				continue
			}
			if string(got.Raw) != line {
				t.Errorf("%s: %q lost its bytes (got %q)", format, line, got.Raw)
			}
		}
	}
}

func TestParsersSurviveHostileInput(t *testing.T) {
	hostile := []string{
		`{"type":"assistant","message":{"content":null}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":{"a":1}}]}}`,
		`{"type":"result","usage":null,"total_cost_usd":null}`,
		`{"type":"stream_event","event":{"delta":null}}`,
		`{"type":"item.completed","item":null}`,
		`{"type":"part","part":{"type":"tool","input":null}}`,
		`{"type":"system","subtype":"init","tools":null,"mcp_servers":null}`,
		`{"type":"system","subtype":"init","mcp_servers":[123,null,{"nope":1}]}`,
		`{"type":1234}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","input":"not-an-object"}]}}`,
	}
	for _, format := range Formats() {
		p := For(format)
		for _, line := range hostile {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s panicked on %q: %v", format, line, r)
					}
				}()
				_ = p.Parse([]byte(line))
			}()
		}
	}
}

// A nil parser keeps plain capture; a do-nothing parser would give an empty timeline.
func TestUnknownFormatHasNoParser(t *testing.T) {
	for _, f := range []string{"", "nope", "claude", "json"} {
		if p := For(f); p != nil {
			t.Errorf("For(%q) returned %T, want nil", f, p)
		}
	}
}

func TestEveryListedFormatHasAParserThatReportsItself(t *testing.T) {
	for _, f := range Formats() {
		p := For(f)
		if p == nil {
			t.Fatalf("no parser for listed format %q", f)
		}
		if p.Format() != f {
			t.Errorf("parser for %q reports Format()=%q", f, p.Format())
		}
	}
}

func TestCostIsNilWhenNotReported(t *testing.T) {
	got := For(FormatClaude).Parse([]byte(`{"type":"result","result":"done"}`))
	if got.Kind != KindResult {
		t.Fatalf("kind = %s", got.Kind)
	}
	if got.Usage.CostUSD != nil {
		t.Errorf("cost = %v, want nil when the agent reported none", *got.Usage.CostUSD)
	}

	// A reported zero is distinct from absent.
	got = For(FormatClaude).Parse([]byte(`{"type":"result","result":"done","total_cost_usd":0}`))
	if got.Usage.CostUSD == nil {
		t.Fatal("a reported cost of 0 was flattened to nil")
	}
	if *got.Usage.CostUSD != 0 {
		t.Errorf("cost = %v, want 0", *got.Usage.CostUSD)
	}
}

// claudeInitFrame is a real init frame from claude-code 2.1.197, trimmed to the keys that matter.
const claudeInitFrame = `{"type":"system","subtype":"init","cwd":"/root",` +
	`"session_id":"333daf46-4a61-466e-95f5-e78ed11987d0",` +
	`"tools":["Task","Bash","Edit"],"mcp_servers":[],"model":"claude-opus-4-8[1m]",` +
	`"permissionMode":"default","apiKeySource":"none","claude_code_version":"2.1.197",` +
	`"output_style":"default","agents":["claude"],"memory_paths":{"auto":"/root/.claude/"}}`

func TestClaudeInitFrameYieldsTheSessionSurface(t *testing.T) {
	got := For(FormatClaude).Parse([]byte(claudeInitFrame))
	if got.Kind != KindSession {
		t.Fatalf("kind = %s, want %s", got.Kind, KindSession)
	}
	if got.SessionID != "333daf46-4a61-466e-95f5-e78ed11987d0" {
		t.Errorf("session id = %q", got.SessionID)
	}
	if got.Model != "claude-opus-4-8[1m]" {
		t.Errorf("model = %q", got.Model)
	}
	if got.AgentVersion != "2.1.197" {
		t.Errorf("version = %q", got.AgentVersion)
	}
	if got.PermissionMode != "default" {
		t.Errorf("permission mode = %q", got.PermissionMode)
	}
	if len(got.Tools) != 3 {
		t.Errorf("tools = %v, want 3", got.Tools)
	}
	if len(got.MCPServers) != 0 {
		t.Errorf("mcp servers = %v, want none", got.MCPServers)
	}
}

// A later system frame must not overwrite the model and tools with empty values.
func TestClaudeNonInitSystemFrameIsUnknown(t *testing.T) {
	got := For(FormatClaude).Parse([]byte(`{"type":"system","subtype":"compact_boundary"}`))
	if got.Kind != KindUnknown {
		t.Errorf("kind = %s, want unknown", got.Kind)
	}
}

func TestClaudeToolUseBecomesAToolStart(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[` +
		`{"type":"text","text":"I'll read the handler."},` +
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"internal/httpapi/dodo.go"}}]}}`
	got := For(FormatClaude).Parse([]byte(line))

	if got.Kind != KindToolStart {
		t.Fatalf("kind = %s, want %s", got.Kind, KindToolStart)
	}
	if got.Tool != "Read" {
		t.Errorf("tool = %q", got.Tool)
	}
	if got.ToolID != "toolu_1" {
		t.Errorf("tool id = %q", got.ToolID)
	}
	if got.Detail != "internal/httpapi/dodo.go" {
		t.Errorf("detail = %q, want the path", got.Detail)
	}
	if !strings.Contains(string(got.Raw), "I'll read the handler.") {
		t.Error("the accompanying text was dropped from Raw")
	}
}

func TestClaudeToolResultClosesTheCallAndFlagsErrors(t *testing.T) {
	ok := For(FormatClaude).Parse([]byte(
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents"}]}}`))
	if ok.Kind != KindToolEnd || ok.ToolID != "toolu_1" {
		t.Errorf("success result = %+v", ok)
	}

	bad := For(FormatClaude).Parse([]byte(
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,` +
			`"content":[{"type":"text","text":"No such file"}]}]}}`))
	if bad.Kind != KindToolFail {
		t.Errorf("error result kind = %s, want %s", bad.Kind, KindToolFail)
	}
	if !strings.Contains(bad.Text, "No such file") {
		t.Errorf("failure text = %q — the reason was lost", bad.Text)
	}
}

func TestClaudeTextAndThinkingAreDistinct(t *testing.T) {
	txt := For(FormatClaude).Parse([]byte(
		`{"type":"assistant","message":{"content":[{"type":"text","text":"the answer"}]}}`))
	if txt.Kind != KindText || txt.Text != "the answer" {
		t.Errorf("text frame = %+v", txt)
	}

	th := For(FormatClaude).Parse([]byte(
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"let me check"}]}}`))
	if th.Kind != KindThinking || th.Text != "let me check" {
		t.Errorf("thinking frame = %+v", th)
	}
}

func TestClaudeResultCarriesUsageAndCost(t *testing.T) {
	line := `{"type":"result","subtype":"success","result":"all done",` +
		`"total_cost_usd":0.1132,"usage":{"input_tokens":12400,"output_tokens":840,` +
		`"cache_read_input_tokens":9000}}`
	got := For(FormatClaude).Parse([]byte(line))

	if got.Kind != KindResult || got.Text != "all done" {
		t.Fatalf("got %+v", got)
	}
	if got.Usage.InputTokens != 12400 || got.Usage.OutputTokens != 840 {
		t.Errorf("tokens = %+v", got.Usage)
	}
	if got.Usage.CacheReadTokens != 9000 {
		t.Errorf("cache read = %d", got.Usage.CacheReadTokens)
	}
	if got.Usage.CostUSD == nil || *got.Usage.CostUSD != 0.1132 {
		t.Errorf("cost = %v", got.Usage.CostUSD)
	}
}

func TestClaudeErrorResultBecomesAnError(t *testing.T) {
	got := For(FormatClaude).Parse([]byte(
		`{"type":"result","subtype":"error","is_error":true,"error":"credit balance too low"}`))
	if got.Kind != KindError {
		t.Fatalf("kind = %s, want %s", got.Kind, KindError)
	}
	if got.Text != "credit balance too low" {
		t.Errorf("text = %q", got.Text)
	}
}

func TestClaudePartialMessageDeltasStream(t *testing.T) {
	got := For(FormatClaude).Parse([]byte(
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"par"}}}`))
	if got.Kind != KindText || got.Text != "par" {
		t.Errorf("delta = %+v", got)
	}
}

func TestToolDetailIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	line, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_use", "id": "t1", "name": "Write",
			"input": map[string]any{"file_path": huge},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := For(FormatClaude).Parse(line)
	if len(got.Detail) > 210 {
		t.Errorf("detail is %d chars, want it bounded", len(got.Detail))
	}
	if !strings.HasSuffix(got.Detail, "…") {
		t.Error("a truncated detail should say so")
	}
}

// Frames observed live from codex-cli 0.147.0.
func TestCodexObservedFramesParse(t *testing.T) {
	p := For(FormatCodex)

	started := p.Parse([]byte(`{"type":"thread.started","thread_id":"019fdd71-98cb-7752-b45b-7ae3304b44dc"}`))
	if started.Kind != KindSession {
		t.Fatalf("thread.started kind = %s, want %s", started.Kind, KindSession)
	}
	if started.SessionID != "019fdd71-98cb-7752-b45b-7ae3304b44dc" {
		t.Errorf("thread id = %q", started.SessionID)
	}

	if got := p.Parse([]byte(`{"type":"turn.started"}`)); got.Kind != KindUnknown {
		t.Errorf("turn.started = %s, want unknown so it is not double-journaled", got.Kind)
	}

	errFrame := p.Parse([]byte(`{"type":"error","message":"Reconnecting... 2/5 (401 Unauthorized)"}`))
	if errFrame.Kind != KindError || !strings.Contains(errFrame.Text, "401") {
		t.Errorf("error frame = %+v", errFrame)
	}
}

func TestCodexPlainTextLogLinesAreKeptNotDropped(t *testing.T) {
	line := `2026-08-07T18:17:18.374793Z ERROR codex_api::endpoint::responses_websocket: failed 401`
	got := For(FormatCodex).Parse([]byte(line))
	if got.Kind != KindUnknown {
		t.Errorf("kind = %s, want unknown", got.Kind)
	}
	if string(got.Raw) != line {
		t.Error("the log line was not preserved; a failed turn's only explanation would be lost")
	}
}

func TestCodexItemFramesBecomeTimelineSteps(t *testing.T) {
	p := For(FormatCodex)

	start := p.Parse([]byte(`{"type":"item.started","item":{"id":"i1","item_type":"command_execution","command":"npm test"}}`))
	if start.Kind != KindToolStart || start.ToolID != "i1" {
		t.Errorf("item.started = %+v", start)
	}
	if start.Detail != "npm test" {
		t.Errorf("detail = %q", start.Detail)
	}

	done := p.Parse([]byte(`{"type":"item.completed","item":{"id":"i1","item_type":"command_execution","command":"npm test"}}`))
	if done.Kind != KindToolEnd || done.ToolID != "i1" {
		t.Errorf("item.completed = %+v", done)
	}

	failed := p.Parse([]byte(`{"type":"item.completed","item":{"id":"i2","item_type":"command_execution","status":"failed","error":"exit 1"}}`))
	if failed.Kind != KindToolFail || failed.Text != "exit 1" {
		t.Errorf("failed item = %+v", failed)
	}

	msg := p.Parse([]byte(`{"type":"item.completed","item":{"item_type":"assistant_message","text":"here you go"}}`))
	if msg.Kind != KindText || msg.Text != "here you go" {
		t.Errorf("assistant message = %+v", msg)
	}
}

func TestCodexTurnCompletedCarriesUsage(t *testing.T) {
	got := For(FormatCodex).Parse([]byte(
		`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20,"cached_input_tokens":5}}`))
	if got.Kind != KindResult {
		t.Fatalf("kind = %s", got.Kind)
	}
	if got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 20 || got.Usage.CacheReadTokens != 5 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if got.Usage.CostUSD != nil {
		t.Error("codex reported no cost, so it must stay nil")
	}
}

// The one frame observed live from opencode-ai 1.18.15.
func TestOpenCodeObservedStepStartCarriesTheSessionID(t *testing.T) {
	got := For(FormatOpenCode).Parse([]byte(
		`{"type":"step_start","timestamp":1786124070031,"sessionID":"ses_022b59abcdef"}`))
	if got.Kind != KindSession {
		t.Fatalf("kind = %s, want %s", got.Kind, KindSession)
	}
	if got.SessionID != "ses_022b59abcdef" {
		t.Errorf("session id = %q", got.SessionID)
	}
}

func TestOpenCodeSessionFrameWithoutAnIDIsUnknown(t *testing.T) {
	got := For(FormatOpenCode).Parse([]byte(`{"type":"step_start","timestamp":1}`))
	if got.Kind != KindUnknown {
		t.Errorf("kind = %s, want unknown", got.Kind)
	}
}

func TestOpenCodePartsBecomeTextAndTools(t *testing.T) {
	p := For(FormatOpenCode)

	txt := p.Parse([]byte(`{"type":"part","part":{"type":"text","text":"hello"}}`))
	if txt.Kind != KindText || txt.Text != "hello" {
		t.Errorf("text part = %+v", txt)
	}

	running := p.Parse([]byte(`{"type":"part","part":{"type":"tool","id":"p1","tool":"bash","state":"running","input":{"command":"ls -la"}}}`))
	if running.Kind != KindToolStart || running.Tool != "bash" || running.Detail != "ls -la" {
		t.Errorf("running tool = %+v", running)
	}

	failed := p.Parse([]byte(`{"type":"part","part":{"type":"tool","id":"p2","tool":"read","state":"error","error":"nope"}}`))
	if failed.Kind != KindToolFail || failed.Text != "nope" {
		t.Errorf("failed tool = %+v", failed)
	}
}

func TestOpenCodeFinishCarriesTokensAndCost(t *testing.T) {
	got := For(FormatOpenCode).Parse([]byte(
		`{"type":"step_finish","text":"done","tokens":{"input":10,"output":2,"cache":1},"cost":0.004}`))
	if got.Kind != KindResult {
		t.Fatalf("kind = %s", got.Kind)
	}
	if got.Usage.InputTokens != 10 || got.Usage.CacheReadTokens != 1 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if got.Usage.CostUSD == nil || *got.Usage.CostUSD != 0.004 {
		t.Errorf("cost = %v", got.Usage.CostUSD)
	}
}

// Run longer with: go test ./internal/extras/agent/agentd/parse/ -run FuzzParsers -fuzz=Fuzz
func FuzzParsers(f *testing.F) {
	f.Add([]byte(claudeInitFrame))
	f.Add([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"ls"}}]}}`))
	f.Add([]byte(`{"type":"result","total_cost_usd":1.5,"usage":{"input_tokens":1}}`))
	f.Add([]byte(`{"type":"thread.started","thread_id":"x"}`))
	f.Add([]byte(`{"type":"item.completed","item":{"item_type":"command_execution"}}`))
	f.Add([]byte(`{"type":"step_start","sessionID":"ses_1"}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`{"type":`))
	f.Add([]byte("\x00\xff{}"))
	f.Add([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","input":[1,2,3]}]}}`))

	f.Fuzz(func(t *testing.T, line []byte) {
		for _, format := range Formats() {
			p := For(format)
			got := p.Parse(line)
			if string(got.Raw) != string(line) {
				t.Fatalf("%s: Raw was altered for %q", format, line)
			}
			if got.Kind == KindToolStart && got.Tool == "" && got.ToolID == "" {
				t.Fatalf("%s: tool step with neither name nor id from %q", format, line)
			}
		}
	})
}
