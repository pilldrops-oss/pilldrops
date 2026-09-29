package parse

import (
	"encoding/json"
	"math"
	"strings"
)

// FormatClaude is Claude Code's `--output-format stream-json --verbose` dialect.
const FormatClaude = "claude-stream-json"

type claudeParser struct{}

func (claudeParser) Format() string { return FormatClaude }

// claudeLine is the envelope every frame shares.
type claudeLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// Init frame.
	SessionID      string   `json:"session_id"`
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
	MCPServers     []any    `json:"mcp_servers"`
	PermissionMode string   `json:"permissionMode"`
	Version        string   `json:"claude_code_version"`

	// Assistant and user frames wrap an Anthropic message.
	Message *claudeMessage `json:"message"`

	// Result frame. TotalCostUSD is a pointer because absent and zero differ.
	Result       string       `json:"result"`
	TotalCostUSD *float64     `json:"total_cost_usd"`
	Usage        *claudeUsage `json:"usage"`
	IsError      bool         `json:"is_error"`
	Error        string       `json:"error"`
	// PermissionDenials lists every tool call the gate refused.
	PermissionDenials []claudeDenial `json:"permission_denials"`
	Delta             *claudeDelta   `json:"delta"`
	Event             *claudeSSEEvt  `json:"event"`
}

type claudeDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content []claudeContent `json:"content"`
	Usage   *claudeUsage    `json:"usage"`
}

type claudeContent struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text"`
	// thinking block
	Thinking string `json:"thinking"`
	// tool_use block
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result block
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Content   any    `json:"content"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type claudeDelta struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

type claudeSSEEvt struct {
	Type  string       `json:"type"`
	Delta *claudeDelta `json:"delta"`
}

// Parse maps one Claude stream-json line onto a Step. A frame with several
// content blocks collapses to the most specific one; the rest stays in Raw.
func (p claudeParser) Parse(line []byte) Step {
	var l claudeLine
	if !decode(line, &l) {
		return unknown(line)
	}

	switch l.Type {
	case "system":
		if l.Subtype != "init" {
			return unknown(line)
		}
		return Step{
			Kind: KindSession, Raw: line,
			SessionID: l.SessionID, Model: l.Model, Tools: l.Tools,
			MCPServers: mcpNames(l.MCPServers), PermissionMode: l.PermissionMode,
			AgentVersion: l.Version,
		}

	case "assistant":
		if l.Message == nil {
			return unknown(line)
		}
		if s, ok := claudeToolStart(l.Message, line); ok {
			return s
		}
		if txt, kind, present := claudeText(l.Message); present {
			// Present-but-empty is deliberate; see claudeText.
			return Step{Kind: kind, Text: txt, Raw: line, Usage: claudeUsageOf(l.Message.Usage)}
		}
		return unknown(line)

	case "user":
		// A tool_result arrives on a user frame; correlate by tool id.
		if l.Message == nil {
			return unknown(line)
		}
		for _, c := range l.Message.Content {
			if c.Type != "tool_result" {
				continue
			}
			kind := KindToolEnd
			if c.IsError {
				kind = KindToolFail
			}
			return Step{
				Kind: kind, ToolID: c.ToolUseID, Raw: line,
				Text: truncate(flatten(c.Content), 400),
			}
		}
		return unknown(line)

	case "result":
		st := Step{Kind: KindResult, Text: l.Result, Raw: line, Usage: claudeUsageOf(l.Usage)}
		for _, d := range l.PermissionDenials {
			// encoding/json matches keys case-insensitively, so an empty entry can
			// appear here; a nameless denial is noise.
			if d.ToolName == "" {
				continue
			}
			st.Denials = append(st.Denials, Denial{
				Tool: d.ToolName, ToolID: d.ToolUseID, Detail: toolDetail(d.ToolInput),
			})
		}
		// The budget adds what it is given, so a negative, NaN or Inf cost would
		// corrupt the cap. Rejected means nil, not zero.
		if v := l.TotalCostUSD; v != nil && *v >= 0 && !math.IsInf(*v, 0) && !math.IsNaN(*v) {
			cost := *v
			st.Usage.CostUSD = &cost
		}
		if l.IsError || l.Subtype == "error" {
			st.Kind = KindError
			if l.Error != "" {
				st.Text = l.Error
			}
		}
		return st

	case "stream_event":
		// Unverified: only emitted with --include-partial-messages, which StreamCmd does not pass.
		d := l.Delta
		if d == nil && l.Event != nil {
			d = l.Event.Delta
		}
		if d == nil {
			return unknown(line)
		}
		if d.Thinking != "" {
			return Step{Kind: KindThinking, Text: d.Thinking, Raw: line}
		}
		if d.Text != "" {
			return Step{Kind: KindText, Text: d.Text, Raw: line}
		}
		return unknown(line)

	case "error":
		return Step{Kind: KindError, Text: firstNonEmpty(l.Error, l.Result), Raw: line}
	}
	return unknown(line)
}

// claudeToolStart extracts a tool_use block, if the message has one.
func claudeToolStart(m *claudeMessage, line []byte) (Step, bool) {
	for _, c := range m.Content {
		if c.Type != "tool_use" {
			continue
		}
		// Neither renderable nor correlatable; let the caller show the raw line.
		if c.Name == "" && c.ID == "" {
			continue
		}
		return Step{
			Kind: KindToolStart, Tool: c.Name, ToolID: c.ID, Raw: line,
			Detail: toolDetail(c.Input),
			Usage:  claudeUsageOf(m.Usage),
		}, true
	}
	return Step{}, false
}

// claudeText collects the prose from a message. The bool reports whether a text
// or thinking block was present, even if empty: redacted reasoning arrives as an
// empty thinking block with a base64 signature, which must not fall through to raw output.
func claudeText(m *claudeMessage) (string, Kind, bool) {
	var text, thinking strings.Builder
	sawText, sawThinking := false, false
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			sawText = true
			text.WriteString(c.Text)
		case "thinking":
			sawThinking = true
			thinking.WriteString(c.Thinking)
		}
	}
	if t := text.String(); t != "" {
		return t, KindText, true
	}
	if sawThinking {
		return thinking.String(), KindThinking, true
	}
	return "", KindText, sawText
}

func claudeUsageOf(u *claudeUsage) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens,
	}
}

// mcpNames pulls server names out of the init frame's mcp_servers.
func mcpNames(raw []any) []string {
	var out []string
	for _, e := range raw {
		switch v := e.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			if n, ok := v["name"].(string); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// toolDetail summarises a tool's input for display; a tool input can be a whole file.
func toolDetail(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	// Most identifying first.
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "prompt", "description"} {
		if v, ok := m[k].(string); ok && v != "" {
			return truncate(v, 200)
		}
	}
	return ""
}

// flatten renders a tool_result's content, a string or a list of content blocks.
func flatten(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
