package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
)

// GateToolName is the tool this server exposes. With GateServerName it forms
// the flag value mcp__<server>__<tool>.
const GateToolName = "approve"

// GateServerName is the key under mcpServers in the config agentd writes.
const GateServerName = "pilldrops"

// GatePermissionTool is the value for --permission-prompt-tool.
var GatePermissionTool = "mcp__" + GateServerName + "__" + GateToolName

// mcpRequest is one JSON-RPC message. id is absent on notifications, which
// must not be answered.
type mcpRequest struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// gateDecision is the payload claude expects, encoded as JSON inside a text
// content block. Returned as the result object directly, the client cannot read it.
type gateDecision struct {
	Behavior string `json:"behavior"`
	// UpdatedInput is required on allow and echoed unchanged, so what runs is
	// what the reviewer saw.
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`
	// Message reaches the model on deny.
	Message string `json:"message,omitempty"`
}

// ServeMCP runs the permission-prompt protocol over one connection. turnID is
// fixed per connection since claude spawns its MCP servers per session. The
// flag is undocumented; the wire format was captured by observation.
func (g *Gate) ServeMCP(ctx context.Context, r io.Reader, w io.Writer, turnID string) error {
	sc := bufio.NewScanner(r)
	// Tool inputs carry whole files, so the scanner's default 64 KiB would truncate.
	sc.Buffer(make([]byte, 0, 64<<10), maxStructuredLine)

	enc := json.NewEncoder(w)
	send := func(v any) error { return enc.Encode(v) }

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req mcpRequest
		if json.Unmarshal([]byte(line), &req) != nil {
			// Unparseable input with no id cannot be answered.
			continue
		}

		switch req.Method {
		case "initialize":
			if err := send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					// Echo the client's version rather than rely on lenient mismatch handling.
					"protocolVersion": clientProtocolVersion(req.Params),
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo": map[string]any{
						"name": GateServerName, "version": "1",
					},
				},
			}); err != nil {
				return err
			}

		case "notifications/initialized", "notifications/cancelled":
			// Notifications take no reply.

		case "tools/list":
			if err := send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []any{gateToolSchema()}},
			}); err != nil {
				return err
			}

		case "tools/call":
			call := parseGateCall(req.Params)
			// An unreadable request is denied, not waved through.
			if call.Tool == "" {
				if err := send(gateResult(req.ID, gateDecision{
					Behavior: "deny",
					Message:  "the approval gate could not read this request, so it was not allowed",
				})); err != nil {
					return err
				}
				continue
			}
			d, msg := g.Request(ctx, turnID, call)
			dec := gateDecision{Behavior: "deny", Message: msg}
			if d == DecisionAllow {
				dec = gateDecision{Behavior: "allow", UpdatedInput: call.Input}
			}
			if err := send(gateResult(req.ID, dec)); err != nil {
				return err
			}

		default:
			if req.ID == nil {
				continue // an unknown notification is not an error
			}
			if err := send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			}); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// gateResult wraps a decision in the content-block shape the client reads.
func gateResult(id any, d gateDecision) map[string]any {
	// Marshal cannot fail here; an empty text block would read as a refusal anyway.
	b, _ := json.Marshal(d)
	return map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(b)}},
		},
	}
}

func gateToolSchema() map[string]any {
	return map[string]any{
		"name": GateToolName,
		"description": "Decide whether a tool call may run. Called automatically before " +
			"each tool use; not for direct invocation.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_name":   map[string]any{"type": "string"},
				"input":       map[string]any{"type": "object"},
				"tool_use_id": map[string]any{"type": "string"},
			},
			// tool_use_id is present in practice but not required.
			"required": []any{"tool_name", "input"},
		},
	}
}

// parseGateCall reads the arguments of a tools/call.
func parseGateCall(params json.RawMessage) ToolCall {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			ToolName  string         `json:"tool_name"`
			Input     map[string]any `json:"input"`
			ToolUseID string         `json:"tool_use_id"`
		} `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ToolCall{}
	}
	return ToolCall{
		Tool: p.Arguments.ToolName, Input: p.Arguments.Input, ID: p.Arguments.ToolUseID,
	}
}

// clientProtocolVersion echoes the version the client announced, defaulting to the
// one observed working.
func clientProtocolVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		return p.ProtocolVersion
	}
	return "2024-11-05"
}
