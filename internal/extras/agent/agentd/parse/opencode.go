package parse

// FormatOpenCode is `opencode run --format json`'s event dialect.
const FormatOpenCode = "opencode-json"

type openCodeParser struct{}

func (openCodeParser) Format() string { return FormatOpenCode }

// openCodeLine is the envelope. Only step_start has been observed live; the
// other frame shapes are unverified.
type openCodeLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Timestamp int64  `json:"timestamp"`

	// Part carries text and tool activity.
	Part *openCodePart `json:"part"`
	Text string        `json:"text"`
	// Tokens and Cost are on a terminal frame.
	Tokens *openCodeTokens `json:"tokens"`
	Cost   *float64        `json:"cost"`
	Error  string          `json:"error"`
}

type openCodePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Tool  string `json:"tool"`
	State string `json:"state"`
	// Input is the tool's arguments, used only for a short label.
	Input map[string]any `json:"input"`
	Error string         `json:"error"`
}

type openCodeTokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Cache  int64 `json:"cache"`
}

// Parse maps one OpenCode event onto a Step.
func (p openCodeParser) Parse(line []byte) Step {
	var l openCodeLine
	if !decode(line, &l) {
		return unknown(line)
	}

	switch l.Type {
	case "step_start", "session", "session_start":
		// The session id is what run -s needs to resume.
		if l.SessionID == "" {
			return unknown(line)
		}
		return Step{Kind: KindSession, SessionID: l.SessionID, Raw: line}

	case "step_finish", "session_finish", "done":
		st := Step{Kind: KindResult, Text: l.Text, Raw: line}
		if l.Tokens != nil {
			st.Usage = Usage{
				InputTokens: l.Tokens.Input, OutputTokens: l.Tokens.Output,
				CacheReadTokens: l.Tokens.Cache,
			}
		}
		if l.Cost != nil {
			v := *l.Cost
			st.Usage.CostUSD = &v
		}
		return st

	case "error":
		return Step{Kind: KindError, Text: l.Error, Raw: line}

	case "part", "part_updated", "message_part":
		return openCodePartStep(l, line)
	}

	// Unknown frame type, but it carries text worth showing.
	if l.Text != "" {
		return Step{Kind: KindText, Text: l.Text, Raw: line}
	}
	return unknown(line)
}

func openCodePartStep(l openCodeLine, line []byte) Step {
	pt := l.Part
	if pt == nil {
		return unknown(line)
	}
	switch pt.Type {
	case "text":
		if pt.Text == "" {
			return unknown(line)
		}
		return Step{Kind: KindText, Text: pt.Text, Raw: line}

	case "reasoning":
		if pt.Text == "" {
			return unknown(line)
		}
		return Step{Kind: KindThinking, Text: pt.Text, Raw: line}

	case "tool", "tool_call", "tool_invocation":
		if pt.Tool == "" && pt.ID == "" {
			return unknown(line)
		}
		detail := ""
		if len(pt.Input) > 0 {
			detail = mapDetail(pt.Input)
		}
		switch pt.State {
		case "running", "pending", "started":
			return Step{Kind: KindToolStart, Tool: pt.Tool, ToolID: pt.ID, Detail: detail, Raw: line}
		case "error", "failed":
			return Step{Kind: KindToolFail, Tool: pt.Tool, ToolID: pt.ID, Detail: detail,
				Text: pt.Error, Raw: line}
		default:
			return Step{Kind: KindToolEnd, Tool: pt.Tool, ToolID: pt.ID, Detail: detail, Raw: line}
		}
	}
	return unknown(line)
}

// mapDetail is toolDetail for an already-decoded input map.
func mapDetail(m map[string]any) string {
	for _, k := range []string{"command", "filePath", "file_path", "path", "pattern", "url", "query"} {
		if v, ok := m[k].(string); ok && v != "" {
			return truncate(v, 200)
		}
	}
	return ""
}
