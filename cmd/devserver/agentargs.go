package main

// Server-side expansion of an agent create's `args` into the same
// files/setup/start the dashboard composes, so CLI/SDK/MCP callers can launch
// task-mode agents. `args` is stripped before the body is forwarded.

import (
	"encoding/json"
	"strings"

	catalogtpl "github.com/pilldrops-oss/pilldrops/internal/templates"
)

// agentTerminalScript serves a browser terminal (ttyd) on $PORT. Keep in sync
// with universalTerminalScript() in web/src/components/launchConfig.tsx.
const agentTerminalScript = `#!/bin/sh
P="${PORT:-8000}"
U="https://github.com/tsl0922/ttyd/releases/download/1.7.7/ttyd.x86_64"
have() { command -v "$1" >/dev/null 2>&1; }
if ! have curl && ! have wget && ! have node; then
  (have apt-get && apt-get update -qq && apt-get install -y -qq curl) >/dev/null 2>&1
  (have apk && apk add --no-cache curl) >/dev/null 2>&1
fi
if [ ! -x /usr/local/bin/ttyd ] && ! have ttyd; then
  if have curl; then curl -fsSL "$U" -o /usr/local/bin/ttyd 2>/dev/null
  elif have wget; then wget -qO /usr/local/bin/ttyd "$U" 2>/dev/null
  fi
  chmod +x /usr/local/bin/ttyd 2>/dev/null
fi
T="$(command -v ttyd || echo /usr/local/bin/ttyd)"
[ -x "$T" ] && exec "$T" -W -p "$P" bash
if have python3; then exec python3 -m http.server "$P"; fi
`

// agentTaskScriptFor runs the task in the background and serves its log on
// $PORT. The task text is read from _pd_task.txt so it never needs shell-escaping.
func agentTaskScriptFor(runCmd string) string {
	cmd := strings.ReplaceAll(runCmd, `"{task}"`, `"$(cat "$HOME"/_pd_task.txt 2>/dev/null || cat _pd_task.txt)"`)
	cmd = strings.ReplaceAll(cmd, "{task}", `$(cat _pd_task.txt)`) // defensive: unquoted placeholder form
	return `#!/bin/sh
have() { command -v "$1" >/dev/null 2>&1; }
( ` + cmd + ` ) >/tmp/agent.log 2>&1 &
if have node; then exec node -e 'const http=require("http"),fs=require("fs");http.createServer((q,s)=>{s.writeHead(200,{"Content-Type":"text/html"});let l="";try{l=fs.readFileSync("/tmp/agent.log","utf8")}catch(e){l="(agent starting...)"}s.end("<!doctype html><meta http-equiv=refresh content=3><title>Agent output</title><body style=\"font-family:ui-monospace,monospace;background:#0a0a0b;color:#e9e9ec;padding:16px\"><pre>"+l.replace(/[<&]/g,c=>c==="<"?"&lt;":"&amp;")+"</pre>")}).listen(process.env.PORT||8000)'; fi
if have python3; then ( echo "agent running — see the CLI/logs"; exec python3 -m http.server "${PORT:-8000}" ); fi
`
}

// expandAgentCreate rewrites an agent create's `args` into files/setup/start.
// Non-agent templates, bodies without args, and explicit starts pass through.
func expandAgentCreate(raw []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	argsAny, ok := obj["args"].(map[string]any)
	if !ok {
		return raw
	}
	tplName, _ := obj["template"].(string)
	t, found := catalogtpl.Resolve(tplName)
	if !found || t.Agent == nil {
		return raw // not an agent template; leave args alone
	}
	// An explicit start command wins.
	if s, _ := obj["start"].(string); s != "" {
		delete(obj, "args")
		out, err := json.Marshal(obj)
		if err != nil {
			return raw
		}
		return out
	}

	task, _ := argsAny["task"].(string)
	mode, _ := argsAny["access_mode"].(string)
	if mode == "" && strings.TrimSpace(task) != "" {
		mode = "task" // a task with no stated mode means "run it"
	}

	files, _ := obj["files"].([]any)
	var script string
	if mode == "task" && strings.TrimSpace(task) != "" {
		script = agentTaskScriptFor(t.Agent.RunCmd)
		files = append(files, map[string]any{"path": "_pd_task.txt", "content": task})
	} else {
		script = agentTerminalScript
	}
	files = append(files, map[string]any{"path": "_pd_agent_start.sh", "content": script})
	obj["files"] = files

	setup, _ := obj["setup"].([]any)
	obj["setup"] = append([]any{t.Agent.InstallCmd}, setup...)

	obj["start"] = "sh _pd_agent_start.sh"
	if _, hasPort := obj["port"]; !hasPort {
		obj["port"] = 8000
	}
	delete(obj, "args")

	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}
