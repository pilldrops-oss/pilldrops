package parse

import (
	"strings"
	"testing"
)

// The property under fuzz is not interpretation but that no input makes a parser
// panic or claim a step it did not read.

func fuzzParsers() map[string]Parser {
	return map[string]Parser{
		"claude":   claudeParser{},
		"codex":    codexParser{},
		"opencode": openCodeParser{},
	}
}

func FuzzParsersNeverPanic(f *testing.F) {
	f.Add(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)
	f.Add(`{"type":"result","total_cost_usd":0.01,"permission_denials":[{"tool_name":"Write"}]}`)
	f.Add(`{"type":"system","subtype":"init","session_id":"x","tools":["Read"]}`)
	f.Add(`{"type":"assistant","message":{"content":[{"type":"redacted_thinking","data":"AAAA"}]}}`)
	f.Add(`{"type":`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(`{"type":"assistant","message":{"content":null}}`)
	f.Add(`{"type":"assistant","message":{"content":[]}}`)
	f.Add(strings.Repeat(`{"a":`, 200)) // deep nesting
	f.Add("\x00\xff\xfe")

	f.Fuzz(func(t *testing.T, in string) {
		for name, p := range fuzzParsers() {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s parser panicked on %q: %v", name, truncFuzz(in), r)
					}
				}()
				st := p.Parse([]byte(in))

				if len(st.Raw) == 0 && len(in) > 0 {
					t.Fatalf("%s dropped Raw for %q", name, truncFuzz(in))
				}

				if st.Kind == KindUnknown {
					if st.Tool != "" || st.ToolID != "" || st.SessionID != "" || len(st.Denials) > 0 {
						t.Fatalf("%s reported detail on an unknown frame %q: %+v",
							name, truncFuzz(in), st)
					}
				}
				if st.Kind == KindToolStart && st.Tool == "" {
					t.Fatalf("%s reported a tool start with no tool name: %q", name, truncFuzz(in))
				}
				for _, d := range st.Denials {
					if d.Tool == "" {
						t.Fatalf("%s reported a denial with no tool: %q", name, truncFuzz(in))
					}
				}
			}()
		}
	})
}

func FuzzClaudeUsageNeverGoesNegative(f *testing.F) {
	f.Add(`{"type":"result","total_cost_usd":0.5}`)
	f.Add(`{"type":"result","total_cost_usd":-1}`)
	f.Add(`{"type":"result","total_cost_usd":1e308}`)
	f.Add(`{"type":"result","total_cost_usd":"not a number"}`)

	f.Fuzz(func(t *testing.T, in string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked on %q: %v", truncFuzz(in), r)
			}
		}()
		st := claudeParser{}.Parse([]byte(in))
		if st.Usage.CostUSD != nil && *st.Usage.CostUSD < 0 {
			t.Fatalf("negative cost %v from %q — a spend cap cannot be enforced against it",
				*st.Usage.CostUSD, truncFuzz(in))
		}
	})
}

func truncFuzz(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
