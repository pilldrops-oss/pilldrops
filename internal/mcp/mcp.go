// Package mcp implements a stateless Model Context Protocol server over the
// /v1 API, served over Streamable HTTP (HTTPServer) and stdio (the CLI).
package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ServerVersion is reported in the initialize handshake. Keep in step with internal/cli.Version.
const ServerVersion = "1.8.1"

// protocolVersions are accepted newest first; initialize echoes the client's if supported.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Backend performs one platform API call on behalf of a tool. status is 0 when err != nil.
type Backend func(method, path string, body []byte, contentType string) (status int, respBody []byte, err error)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(id json.RawMessage, result any) []byte {
	b, _ := json.Marshal(response{JSONRPC: "2.0", ID: rawID(id), Result: result})
	return b
}

func replyErr(id json.RawMessage, code int, msg string) []byte {
	b, _ := json.Marshal(response{JSONRPC: "2.0", ID: rawID(id), Error: &rpcError{Code: code, Message: msg}})
	return b
}

// rawID preserves the caller's id type, number or string.
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return json.RawMessage(id)
}

// HandleMessage processes one JSON-RPC message or batch and returns the
// serialized response, or nil for notifications.
func HandleMessage(raw []byte, be Backend) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var msgs []json.RawMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return replyErr(nil, -32700, "parse error: "+err.Error())
		}
		var out []json.RawMessage
		for _, m := range msgs {
			if r := handleOne(m, be); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		b, _ := json.Marshal(out)
		return b
	}
	return handleOne(raw, be)
}

func handleOne(raw []byte, be Backend) []byte {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return replyErr(nil, -32700, "parse error: "+err.Error())
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := protocolVersions[0]
		for _, v := range protocolVersions {
			if p.ProtocolVersion == v {
				ver = v
				break
			}
		}
		return reply(req.ID, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pilldrops", "version": ServerVersion},
			"instructions": "PillDrops gives agents disposable cloud sandboxes. Typical flow: " +
				"create_sandbox (template python-3.11) → write_file / run_command → read output → " +
				"destroy_sandbox. App servers: create_sandbox with start+ports (the app MUST bind " +
				"the injected $PORT env var) → the returned url is a public preview. Debug with " +
				"get_logs / get_metrics; past sandboxes (with cost) via the CLI `pilldrops history`. " +
				"Before anything risky, save_checkpoint and roll back with restore_sandbox; create_backup " +
				"outlives the sandbox, and save_snapshot turns a prepared sandbox into a template others boot from.",
		})
	case "ping":
		return reply(req.ID, map[string]any{})
	case "tools/list":
		return reply(req.ID, map[string]any{"tools": toolDefs()})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return replyErr(req.ID, -32602, "invalid params: "+err.Error())
		}
		res := callTool(p.Name, p.Arguments, be)
		return reply(req.ID, res)
	default:
		return replyErr(req.ID, -32601, "method not found: "+req.Method)
	}
}

// toolText wraps a payload as an MCP tool result; JSON objects also go in structuredContent.
func toolText(body []byte) map[string]any {
	res := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(body)}},
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		if _, isObj := v.(map[string]any); isObj {
			res["structuredContent"] = v
		}
	}
	return res
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func apiCall(be Backend, method, path string, body []byte, ctype string) ([]byte, string) {
	status, resp, err := be(method, path, body, ctype)
	if err != nil {
		return nil, "network error calling the PillDrops API: " + err.Error() +
			" — check connectivity (the CLI equivalent is `pilldrops doctor`)"
	}
	if status < 200 || status > 299 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(resp, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", status)
		}
		hint := ""
		switch {
		case status == 401:
			hint = " — missing/invalid API key. HTTP transport: send 'Authorization: Bearer <key>' " +
				"(dashboard → API keys). stdio transport: run `pilldrops login --key <key>` first."
		case status == 404:
			hint = " — sandbox gone or id wrong; list_sandboxes shows active ones."
		case status == 402 || status == 403:
			hint = " — plan/limit issue; get_usage shows consumption vs caps."
		case status >= 500:
			hint = " — server-side; retry once before giving up."
		}
		return nil, fmt.Sprintf("%s (HTTP %d)%s", e.Error, status, hint)
	}
	return resp, ""
}

type tool struct {
	name, desc string
	schema     map[string]any
	run        func(a map[string]any, be Backend) map[string]any
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(t, desc string) map[string]any { return map[string]any{"type": t, "description": desc} }

func str(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// num reads an integer argument, 0 if absent or unparseable. Strings are
// accepted because LLM clients routinely send "8080" for an integer field.
func num(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	}
	return 0
}

func toolList() []tool {
	simpleGet := func(pathf string) func(map[string]any, Backend) map[string]any {
		return func(a map[string]any, be Backend) map[string]any {
			p := pathf
			if strings.Contains(pathf, "%s") {
				p = fmt.Sprintf(pathf, url.PathEscape(str(a, "sandbox_id")))
			}
			body, errText := apiCall(be, "GET", p, nil, "")
			if errText != "" {
				return toolError(errText)
			}
			return toolText(body)
		}
	}
	simplePost := func(pathf string) func(map[string]any, Backend) map[string]any {
		return func(a map[string]any, be Backend) map[string]any {
			body, errText := apiCall(be, "POST", fmt.Sprintf(pathf, url.PathEscape(str(a, "sandbox_id"))), nil, "")
			if errText != "" {
				return toolError(errText)
			}
			return toolText(body)
		}
	}
	idSchema := obj(map[string]any{"sandbox_id": prop("string", "sandbox id, e.g. sb_ab12cd")}, "sandbox_id")

	return append([]tool{
		{
			name:   "list_templates",
			desc:   "List available sandbox templates (python-3.11, node, playwright, …) with their default resources.",
			schema: obj(map[string]any{}),
			run:    simpleGet("/v1/templates"),
		},
		{
			name: "create_sandbox",
			desc: "Create an isolated cloud sandbox. Returns {id, status, url?}. For app servers pass start " +
				"(long-lived command) and ports — the app MUST listen on the injected $PORT env var, and the " +
				"returned url is a public preview. Destroy sandboxes when done — a running sandbox holds\n" +
				"a concurrency slot and keeps metering.",
			schema: obj(map[string]any{
				"template":        prop("string", "template id (see list_templates); default python-3.11"),
				"image":           prop("string", "custom docker image instead of a template"),
				"start":           prop("string", "long-lived app/server command; makes the sandbox serve a preview URL"),
				"ports":           map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "preview ports (app must bind $PORT)"},
				"env":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "environment variables, KEY=VAL"},
				"vcpu":            prop("integer", "vCPUs (0 = template default)"),
				"memory_mb":       prop("integer", "memory MB (0 = template default)"),
				"storage_mb":      prop("integer", "disk MB (0 = template default)"),
				"timeout_seconds": prop("integer", "sandbox lifetime (0 = plan default)"),
				"idle_seconds":    prop("integer", "idle auto-pause; -1 = never pause (always-on servers)"),
				"runtime":         prop("string", "pin isolation runtime: firecracker | docker (omit = auto)"),
				"public":          prop("boolean", "make the preview URL reachable without a token (shareable link); default false = private"),
				"slug":            prop("string", "DNS label this sandbox is hosted and resolved under; also its name on a network tag (<slug>.pd)"),
				"networks": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "opt-in network tags. Sandboxes cannot reach each other by default; two in the same org " +
						"on the same node reach each other only if they share a tag, and resolve each other as <slug>.pd"},
			}),
			run: func(a map[string]any, be Backend) map[string]any {
				req := map[string]any{"client": "mcp"}
				if v := str(a, "template"); v != "" {
					req["template"] = v
				}
				for argName, apiName := range map[string]string{"image": "image", "start": "start", "runtime": "runtime", "slug": "slug"} {
					if v := str(a, argName); v != "" {
						req[apiName] = v
					}
				}
				for argName, apiName := range map[string]string{"vcpu": "vcpu", "memory_mb": "memory_mb",
					"storage_mb": "storage_mb", "timeout_seconds": "timeout", "idle_seconds": "idle_timeout"} {
					if v := num(a, argName); v != 0 {
						req[apiName] = v
					}
				}
				if v, ok := a["ports"].([]any); ok && len(v) > 0 {
					req["ports"] = v
				}
				if v, ok := a["env"].([]any); ok && len(v) > 0 {
					req["env"] = v
				}
				if v, ok := a["networks"].([]any); ok && len(v) > 0 {
					req["networks"] = v
				}
				if v, ok := a["public"].(bool); ok && v {
					req["public"] = true
				}
				if req["template"] == nil && req["image"] == nil {
					req["template"] = "python-3.11"
				}
				body, _ := json.Marshal(req)
				resp, errText := apiCall(be, "POST", "/v1/sandboxes", body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name: "set_networks",
			desc: "Replace a sandbox's network tags. Sandboxes cannot reach each other by default; two in the " +
				"same org on the same node reach each other, on any port, only while they share a tag, and " +
				"resolve each other as <slug>.pd. Takes effect at once, no restart. An empty list closes the " +
				"sandbox off again. Same node only: a peer elsewhere neither resolves nor connects.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id or slug"),
				"networks": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "the tags the sandbox should be on; [] removes it from every network"},
			}, "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				nets := []any{}
				if v, ok := a["networks"].([]any); ok {
					nets = v
				}
				body, _ := json.Marshal(map[string]any{"networks": nets})
				resp, errText := apiCall(be, "PATCH", sbPath(a, ""), body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name:   "list_sandboxes",
			desc:   "List active sandboxes: id, template, status.",
			schema: obj(map[string]any{}),
			run:    simpleGet("/v1/sandboxes"),
		},
		{
			name:   "get_sandbox",
			desc:   "Inspect one sandbox: status, resources, preview url when it serves an app.",
			schema: idSchema,
			run:    simpleGet("/v1/sandboxes/%s"),
		},
		{
			name: "run_command",
			desc: "Run a shell command in the sandbox and get {stdout, stderr, exit_code}. Use " +
				"bash -c '…' style strings for pipelines. Large output is truncated — write to a file " +
				"and read_file it instead.",
			schema: obj(map[string]any{
				"sandbox_id":      prop("string", "sandbox id"),
				"cmd":             prop("string", "shell command to run"),
				"cwd":             prop("string", "working directory (default workspace root)"),
				"timeout_seconds": prop("integer", "command timeout (default 60)"),
			}, "sandbox_id", "cmd"),
			run: func(a map[string]any, be Backend) map[string]any {
				req := map[string]any{"cmd": str(a, "cmd")}
				if v := str(a, "cwd"); v != "" {
					req["cwd"] = v
				}
				if v := num(a, "timeout_seconds"); v != 0 {
					req["timeout"] = v
				}
				body, _ := json.Marshal(req)
				resp, errText := apiCall(be, "POST", sbPath(a, "/exec"), body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name: "start_server",
			desc: "Register + launch a persistent server command in the sandbox (survives idle-pause/resume, " +
				"unlike run_command). The app must bind $PORT.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"cmd":        prop("string", "server command, e.g. `python /app/main.py`"),
				"port":       prop("integer", "port the app listens on (0 = the sandbox default)"),
			}, "sandbox_id", "cmd"),
			run: func(a map[string]any, be Backend) map[string]any {
				body, _ := json.Marshal(map[string]any{"cmd": str(a, "cmd"), "port": num(a, "port")})
				resp, errText := apiCall(be, "POST", sbPath(a, "/start"), body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name: "write_file",
			desc: "Write text content to a file in the sandbox workspace (path is relative to the workspace root; parent dirs are created).",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"path":       prop("string", "file path relative to the workspace, e.g. app/main.py"),
				"content":    prop("string", "full file content"),
			}, "sandbox_id", "path", "content"),
			run: func(a map[string]any, be Backend) map[string]any {
				p := sbPath(a, "/files?path="+url.QueryEscape(strings.TrimPrefix(str(a, "path"), "/")))
				_, errText := apiCall(be, "POST", p, []byte(str(a, "content")), "application/octet-stream")
				if errText != "" {
					return toolError(errText)
				}
				written, _ := json.Marshal(map[string]string{"written": str(a, "path")})
				return toolText(written)
			},
		},
		{
			name: "read_file",
			desc: "Read a file from the sandbox workspace and return its content as text.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"path":       prop("string", "file path relative to the workspace"),
			}, "sandbox_id", "path"),
			run: func(a map[string]any, be Backend) map[string]any {
				p := sbPath(a, "/files?path="+url.QueryEscape(strings.TrimPrefix(str(a, "path"), "/")))
				resp, errText := apiCall(be, "GET", p, nil, "")
				if errText != "" {
					return toolError(errText)
				}
				return map[string]any{"content": []map[string]any{{"type": "text", "text": string(resp)}}}
			},
		},
		{
			name: "get_logs",
			desc: "Fetch recent sandbox logs (start command + service output) — the first stop when an app misbehaves or a preview 502s.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"limit":      prop("integer", "max lines (default 500)"),
			}, "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				limit := num(a, "limit")
				if limit == 0 {
					limit = 500
				}
				resp, errText := apiCall(be, "GET", sbPath(a, fmt.Sprintf("/logs?limit=%d", limit)), nil, "")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name:   "get_metrics",
			desc:   "Live CPU/memory of a sandbox — spot OOMs and busy loops.",
			schema: idSchema,
			run:    simpleGet("/v1/sandboxes/%s/metrics"),
		},
		{
			name:   "pause_sandbox",
			desc:   "Pause a sandbox: metering stops and its slot is freed, state is kept. resume_sandbox wakes it.",
			schema: idSchema,
			run:    simplePost("/v1/sandboxes/%s/pause"),
		},
		{
			name: "fork_sandbox",
			desc: "Fork a sandbox: a running copy with the same disk, memory and processes, under a new id. " +
				"Use it to try something risky on a copy, run several attempts from one state, or reproduce a bug without touching the original.",
			schema: idSchema,
			run:    simplePost("/v1/sandboxes/%s/fork"),
		},
		{
			name:   "resume_sandbox",
			desc:   "Resume a paused sandbox (also relaunches its registered start_server command).",
			schema: idSchema,
			run:    simplePost("/v1/sandboxes/%s/resume"),
		},
		{
			name: "get_events",
			desc: "Recent lifecycle events for the org, newest first: created/paused/resumed/destroyed and " +
				"`oom` (a process was out-of-memory-killed inside a sandbox). Watch this to notice a runaway " +
				"process getting reaped. Pass sandbox_id to filter to one sandbox.",
			schema: obj(map[string]any{"sandbox_id": prop("string", "optional: only this sandbox's events")}),
			run: func(a map[string]any, be Backend) map[string]any {
				body, errText := apiCall(be, "GET", "/v1/events?limit=200", nil, "")
				if errText != "" {
					return toolError(errText)
				}
				sid := str(a, "sandbox_id")
				if sid == "" {
					return toolText(body)
				}
				var resp struct {
					Events []map[string]any `json:"events"`
				}
				_ = json.Unmarshal(body, &resp)
				filtered := make([]map[string]any, 0, len(resp.Events))
				for _, e := range resp.Events {
					if s, _ := e["sandbox_id"].(string); s == sid {
						filtered = append(filtered, e)
					}
				}
				out, _ := json.Marshal(map[string]any{"events": filtered})
				return toolText(out)
			},
		},
		{
			name: "wait_ready",
			desc: "Block until the sandbox's app answers on a port (or timeout), by probing 127.0.0.1:<port> " +
				"from inside the sandbox. Returns {ready, port, code}. Use after start_server, before hitting the preview url.",
			schema: obj(map[string]any{
				"sandbox_id":      prop("string", "sandbox id"),
				"port":            prop("integer", "app port (default 8000)"),
				"timeout_seconds": prop("integer", "max seconds to wait (default 30)"),
			}, "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				sid := url.PathEscape(str(a, "sandbox_id"))
				port := num(a, "port")
				if port == 0 {
					port = 8000
				}
				budget := num(a, "timeout_seconds")
				if budget == 0 {
					budget = 30
				}
				probe, _ := json.Marshal(map[string]any{
					"cmd":     fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' 127.0.0.1:%d 2>/dev/null || echo 000", port),
					"timeout": 5,
				})
				// Bound wall clock, not attempts; each probe is an exec with its own 5s timeout.
				deadline := time.Now().Add(time.Duration(budget) * time.Second)
				for {
					resp, errText := apiCall(be, "POST", "/v1/sandboxes/"+sid+"/exec", probe, "application/json")
					if errText == "" {
						var r struct {
							Stdout string `json:"stdout"`
						}
						_ = json.Unmarshal(resp, &r)
						code := strings.TrimSpace(r.Stdout)
						if len(code) == 3 && (code[0] == '2' || code[0] == '3') {
							out, _ := json.Marshal(map[string]any{"ready": true, "port": port, "code": code})
							return toolText(out)
						}
					}
					if !time.Now().Add(time.Second).Before(deadline) {
						break
					}
					time.Sleep(time.Second)
				}
				out, _ := json.Marshal(map[string]any{"ready": false, "port": port})
				return toolText(out)
			},
		},
		{
			name:   "destroy_sandbox",
			desc:   "Destroy a sandbox and stop entitlements. Its record, logs and cost remain in history.",
			schema: idSchema,
			run: func(a map[string]any, be Backend) map[string]any {
				id := str(a, "sandbox_id")
				if id == "" {
					// An empty id would dispatch DELETE /v1/sandboxes/ and surface a misleading 404.
					return toolError(`destroy_sandbox requires a "sandbox_id" — the id returned by create_sandbox`)
				}
				resp, errText := apiCall(be, "DELETE", "/v1/sandboxes/"+url.PathEscape(id), nil, "")
				if errText != "" {
					return toolError(errText)
				}
				if len(resp) == 0 {
					resp = []byte(`{"destroyed":true}`)
				}
				return toolText(resp)
			},
		},
		{
			name:   "get_usage",
			desc:   "Account usage and charges — current consumption vs plan limits.",
			schema: obj(map[string]any{}),
			run:    simpleGet("/v1/usage"),
		},
	}, platformTools()...)
}

func toolDefs() []map[string]any {
	var defs []map[string]any
	for _, t := range toolList() {
		defs = append(defs, map[string]any{
			"name": t.name, "description": t.desc, "inputSchema": t.schema,
		})
	}
	return defs
}

func callTool(name string, args map[string]any, be Backend) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	for _, t := range toolList() {
		if t.name == name {
			return t.run(args, be)
		}
	}
	return toolError("unknown tool: " + name + " (tools/list shows the catalog)")
}
