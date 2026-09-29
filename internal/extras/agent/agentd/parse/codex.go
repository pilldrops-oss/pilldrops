package parse

import "encoding/json"

// FormatCodex is `codex exec --json`'s JSONL dialect.
const FormatCodex = "codex-json"

type codexParser struct{}

func (codexParser) Format() string { return FormatCodex }

// codexLine is the envelope: a type plus top-level fields.
type codexLine struct {
	Type string `json:"type"`

	// ThreadID on thread.started is what exec resume needs.
	ThreadID string `json:"thread_id"`
	// Message is set on error frames.
	Message string `json:"message"`

	// Item and Usage shapes are unverified against a live agent.
	Item *codexItem `json:"item"`
	// Usage is on the terminal frame.
	Usage *codexUsage `json:"usage"`
	Text  string      `json:"text"`
	Delta string      `json:"delta"`
}

type codexItem struct {
	ID   string `json:"id"`
	Type string `json:"item_type"`
	// Type2 is an alternate spelling of item_type.
	Type2   string          `json:"type"`
	Text    string          `json:"text"`
	Command string          `json:"command"`
	Name    string          `json:"name"`
	Status  string          `json:"status"`
	Args    json.RawMessage `json:"arguments"`
	Error   string          `json:"error"`
}

type codexUsage struct {
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CachedTokens int64    `json:"cached_input_tokens"`
	CostUSD      *float64 `json:"cost_usd"`
}

// Parse maps one codex JSONL line onto a Step. Codex interleaves plain-text log
// lines with its JSONL; those come back as KindUnknown and are journaled as output.
func (p codexParser) Parse(line []byte) Step {
	var l codexLine
	if !decode(line, &l) {
		return unknown(line)
	}

	switch l.Type {
	case "thread.started":
		return Step{Kind: KindSession, SessionID: l.ThreadID, Raw: line}

	case "turn.started":
		// The supervisor already journals its own turn.started.
		return unknown(line)

	case "error":
		return Step{Kind: KindError, Text: l.Message, Raw: line}

	case "turn.completed", "turn.finished":
		st := Step{Kind: KindResult, Text: firstNonEmpty(l.Text, l.Message), Raw: line}
		if l.Usage != nil {
			st.Usage = Usage{
				InputTokens: l.Usage.InputTokens, OutputTokens: l.Usage.OutputTokens,
				CacheReadTokens: l.Usage.CachedTokens, CostUSD: l.Usage.CostUSD,
			}
		}
		return st

	case "turn.failed":
		return Step{Kind: KindError, Text: firstNonEmpty(l.Message, l.Text), Raw: line}

	case "item.started", "item.completed", "item.updated":
		return codexItemStep(l, line)

	case "item.delta", "output_text.delta":
		if txt := firstNonEmpty(l.Delta, l.Text); txt != "" {
			return Step{Kind: KindText, Text: txt, Raw: line}
		}
		return unknown(line)
	}
	return unknown(line)
}

func codexItemStep(l codexLine, line []byte) Step {
	it := l.Item
	if it == nil {
		return unknown(line)
	}
	kind := firstNonEmpty(it.Type, it.Type2)
	name := firstNonEmpty(it.Name, kind)

	switch kind {
	case "assistant_message", "message", "agent_message":
		if it.Text == "" {
			return unknown(line)
		}
		return Step{Kind: KindText, Text: it.Text, Raw: line}

	case "reasoning", "thinking":
		if it.Text == "" {
			return unknown(line)
		}
		return Step{Kind: KindThinking, Text: it.Text, Raw: line}

	case "command_execution", "local_shell_call", "function_call", "tool_call", "mcp_tool_call":
		if name == "" && it.ID == "" {
			return unknown(line)
		}
		detail := firstNonEmpty(it.Command, toolDetail(it.Args))
		switch {
		case l.Type == "item.started":
			return Step{Kind: KindToolStart, Tool: name, ToolID: it.ID, Detail: truncate(detail, 200), Raw: line}
		case it.Status == "failed" || it.Error != "":
			return Step{Kind: KindToolFail, Tool: name, ToolID: it.ID,
				Detail: truncate(detail, 200), Text: it.Error, Raw: line}
		default:
			return Step{Kind: KindToolEnd, Tool: name, ToolID: it.ID, Detail: truncate(detail, 200), Raw: line}
		}
	}
	return unknown(line)
}
