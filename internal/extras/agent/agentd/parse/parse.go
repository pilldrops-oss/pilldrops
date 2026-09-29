// Package parse turns an agent CLI's JSONL output into dialect-agnostic steps.
// An unrecognised line becomes KindUnknown carrying its original bytes, so it
// degrades to visible text rather than being dropped.
package parse

import (
	"bytes"
	"encoding/json"
)

// Kind is what a parsed line means, independent of dialect.
type Kind string

const (
	// KindUnknown is a line the parser did not recognise; Raw is preserved.
	KindUnknown Kind = "unknown"
	// KindSession is the opening frame describing the run.
	KindSession Kind = "session"
	// KindText is visible assistant output.
	KindText Kind = "text"
	// KindThinking is reasoning output.
	KindThinking Kind = "thinking"
	// KindToolStart, KindToolEnd and KindToolFail are the timeline.
	KindToolStart Kind = "tool_start"
	KindToolEnd   Kind = "tool_end"
	KindToolFail  Kind = "tool_fail"
	// KindResult is the terminal frame carrying the final answer and usage.
	KindResult Kind = "result"
	// KindError is an error the agent reported without ending the run.
	KindError Kind = "error"
)

// Usage is per-turn token accounting. CostUSD is nil when the agent reported no
// cost; it is never fabricated as zero.
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	CostUSD         *float64
}

// Denial is one tool call refused by the approval gate.
type Denial struct {
	Tool   string
	ToolID string
	// Detail may carry secrets; redaction happens at the journal writer.
	Detail string
}

// Step is one parsed line.
type Step struct {
	Kind Kind
	// Text carries visible output, reasoning, or an error message.
	Text string
	// ToolID correlates a start with its end; agents interleave calls.
	Tool   string
	ToolID string
	// Detail summarises what the tool acted on. It may carry secrets; redaction
	// happens at the journal writer.
	Detail string
	// Session metadata, present on KindSession.
	SessionID      string
	Model          string
	Tools          []string
	MCPServers     []string
	PermissionMode string
	AgentVersion   string

	// Denials are tool calls the approval gate blocked, present on KindResult.
	Denials []Denial

	Usage Usage
	// Raw is the original line, always set.
	Raw []byte
}

// Parser converts one line of an agent's output into a Step.
type Parser interface {
	// Parse never fails; an unparseable line is KindUnknown with Raw set.
	Parse(line []byte) Step
	// Format is the dialect name, matching AgentMeta.StreamFormat.
	Format() string
}

// For returns the parser for a StreamFormat, or nil for an unknown dialect.
func For(format string) Parser {
	switch format {
	case FormatClaude:
		return claudeParser{}
	case FormatCodex:
		return codexParser{}
	case FormatOpenCode:
		return openCodeParser{}
	default:
		return nil
	}
}

// Formats lists every dialect with a parser.
func Formats() []string { return []string{FormatClaude, FormatCodex, FormatOpenCode} }

func unknown(line []byte) Step {
	return Step{Kind: KindUnknown, Raw: line}
}

// looksJSON is a cheap first-byte check; codex interleaves plain log lines with its JSONL.
func looksJSON(line []byte) bool {
	t := bytes.TrimSpace(line)
	return len(t) > 0 && t[0] == '{'
}

func decode(line []byte, v any) bool {
	return looksJSON(line) && json.Unmarshal(bytes.TrimSpace(line), v) == nil
}
