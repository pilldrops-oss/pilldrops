package mcp

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// The platform tools beyond the create/run/destroy core: captures
// (checkpoints, backups, named snapshots), lifetime, directory listing, org
// secrets, raw port forwards and custom domains. Split from mcp.go for size
// only — toolList appends them, and they use its helpers and its failure shape.

// sbPath is /v1/sandboxes/<escaped id><tail>.
func sbPath(a map[string]any, tail string) string {
	return "/v1/sandboxes/" + url.PathEscape(str(a, "sandbox_id")) + tail
}

// truthy reads a confirmation flag. Strings are accepted for the same reason
// num accepts them: LLM clients routinely send "true" for a boolean field.
func truthy(a map[string]any, k string) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// call issues one API call and renders it. A nil body sends nothing; anything
// else is marshalled as JSON. An empty 2xx body (the 204s) becomes a status
// object, so a success never reads as an empty answer.
func call(be Backend, method, path string, body any) map[string]any {
	var raw []byte
	ctype := ""
	if body != nil {
		raw, _ = json.Marshal(body)
		ctype = "application/json"
	}
	resp, errText := apiCall(be, method, path, raw, ctype)
	if errText != "" {
		return toolError(errText)
	}
	if len(resp) == 0 {
		resp = []byte(`{"status":"ok"}`)
	}
	return toolText(resp)
}

// savedStateTarget resolves the checkpoint/backup pair into the path segment
// that addresses it. Exactly one must be given: they are different stores, and
// guessing which one the caller meant would restore or delete the wrong thing.
func savedStateTarget(a map[string]any) (tail string, errText string) {
	name, bkp := strings.TrimSpace(str(a, "checkpoint")), strings.TrimSpace(str(a, "backup"))
	switch {
	case name != "" && bkp != "":
		return "", `give either "checkpoint" or "backup", not both — they are different stores`
	case name != "":
		return "/checkpoints/" + url.PathEscape(name), ""
	case bkp != "":
		return "/backups/" + url.PathEscape(bkp), ""
	}
	return "", `name the save point: "checkpoint" (a name from save_checkpoint) or "backup" ` +
		`(an id from create_backup) — list_saved_states shows both`
}

func platformTools() []tool {
	sandboxArg := func(extra map[string]any) map[string]any {
		props := map[string]any{"sandbox_id": prop("string", "sandbox id, e.g. sb_ab12cd")}
		for k, v := range extra {
			props[k] = v
		}
		return props
	}
	savedStateArgs := map[string]any{
		"checkpoint": prop("string", "a checkpoint name (on-node save point)"),
		"backup":     prop("string", "a backup id (durable off-node copy)"),
		"confirm":    prop("boolean", "must be true; without it nothing happens"),
	}

	return []tool{
		{
			name: "save_checkpoint",
			desc: "Save a named, restorable point of a running sandbox — its disk, plus its RAM and " +
				"running processes on Firecracker — on the node that hosts it. Cheap: the guest is frozen " +
				"for a fraction of a second (the reply reports paused_ms) and then runs on. Reach for it " +
				"before anything you might want to undo — a migration, a risky refactor, an install — then " +
				"roll back with restore_sandbox. A checkpoint lives on the node and dies with the sandbox; " +
				"create_backup is the copy that outlives it, save_snapshot the one other sandboxes can boot " +
				"from. There is a per-sandbox cap, which list_saved_states reports.",
			schema: obj(sandboxArg(map[string]any{
				"name": prop("string", "checkpoint name, e.g. before-migration"),
			}), "sandbox_id", "name"),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "POST", sbPath(a, "/checkpoints"), map[string]any{"name": str(a, "name")})
			},
		},
		{
			name: "create_backup",
			desc: "Start a durable backup of a sandbox to the platform's object store. The sandbox is " +
				"frozen only for the capture and keeps running; the upload then continues in the background, " +
				"so this returns a backup whose status is pending and which can take minutes to become " +
				"ready — poll list_saved_states. Unlike a checkpoint, a backup survives the sandbox and the " +
				"node. Use it before destroying something you may need back, or on a long-lived workstation. " +
				"Answers 501 when the deployment has no object store configured.",
			schema: obj(sandboxArg(nil), "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "POST", sbPath(a, "/backups"), nil)
			},
		},
		{
			name: "list_saved_states",
			desc: "Everything this sandbox can be rolled back to: its on-node checkpoints (with the cap on " +
				"how many it may hold) and its durable backups, with each one's status, size and age. Call it " +
				"before restore_sandbox to find the name or id to restore, or after create_backup to watch a " +
				"pending backup turn ready. A deployment with no object store reports backups_unavailable and " +
				"still lists checkpoints.",
			schema: obj(sandboxArg(nil), "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				cps, errText := apiCall(be, "GET", sbPath(a, "/checkpoints"), nil, "")
				if errText != "" {
					return toolError(errText)
				}
				out := map[string]any{}
				_ = json.Unmarshal(cps, &out)
				// Backups are a separate store and a separate failure: a node
				// without one still has checkpoints, and saying so beats
				// failing the whole read.
				if bks, errText := apiCall(be, "GET", sbPath(a, "/backups"), nil, ""); errText != "" {
					out["backups_unavailable"] = errText
				} else {
					var b map[string]any
					_ = json.Unmarshal(bks, &b)
					out["backups"] = b["backups"]
				}
				body, _ := json.Marshal(out)
				return toolText(body)
			},
		},
		{
			name: "restore_sandbox",
			desc: "Roll a sandbox back onto one of its save points, in place. DESTRUCTIVE: everything " +
				"written since that save is gone, and the running processes are replaced by the captured " +
				"ones. The id, URL and token do not change, so anything pointing at the sandbox keeps " +
				"working. Name exactly one of checkpoint (fast, on-node) or backup (pulled from the object " +
				"store, so slower), and pass confirm=true — without it nothing happens.",
			schema: obj(sandboxArg(savedStateArgs), "sandbox_id", "confirm"),
			run: func(a map[string]any, be Backend) map[string]any {
				tail, errText := savedStateTarget(a)
				if errText != "" {
					return toolError(errText)
				}
				if !truthy(a, "confirm") {
					return toolError("restore_sandbox overwrites the sandbox's disk and memory with the " +
						"saved state and cannot be undone — pass confirm=true to go ahead")
				}
				// Only the backup route reads a body; the checkpoint route
				// decodes none, so it is sent none.
				var body any
				if strings.HasPrefix(tail, "/backups/") {
					body = map[string]any{"confirm": true}
				}
				return call(be, "POST", sbPath(a, tail+"/restore"), body)
			},
		},
		{
			name: "delete_saved_state",
			desc: "Delete one save point of a sandbox — a checkpoint (frees node disk, and a slot under the " +
				"checkpoint cap) or a backup (frees object storage). DESTRUCTIVE and irreversible: the state " +
				"is gone and nothing else holds a copy. Name exactly one of checkpoint or backup and pass " +
				"confirm=true. A backup another one was deduplicated against is refused rather than silently " +
				"breaking it.",
			schema: obj(sandboxArg(savedStateArgs), "sandbox_id", "confirm"),
			run: func(a map[string]any, be Backend) map[string]any {
				tail, errText := savedStateTarget(a)
				if errText != "" {
					return toolError(errText)
				}
				if !truthy(a, "confirm") {
					return toolError("delete_saved_state destroys the only copy of that state — " +
						"pass confirm=true to go ahead")
				}
				return call(be, "DELETE", sbPath(a, tail), nil)
			},
		},
		{
			name: "set_backup_schedule",
			desc: "Put a sandbox on an automatic backup schedule, or take it off one. Give period (a " +
				"duration like 24h or 30m — whole minutes under an hour, or whole hours up to 24h, and never " +
				"under 15m) or cron (a 5-field expression), not both; \"off\" as either removes the schedule. " +
				"retention (like 7d) is how long each backup is kept, and needs a period. Every run costs " +
				"what create_backup costs, so do not schedule one on a sandbox that will not be around.",
			schema: obj(sandboxArg(map[string]any{
				"period":    prop("string", "duration between backups, e.g. 24h; or \"off\" to remove the schedule"),
				"cron":      prop("string", "5-field cron expression instead of period"),
				"retention": prop("string", "how long backups are kept, e.g. 7d (needs a period)"),
			}), "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				sched := map[string]any{}
				for _, k := range []string{"period", "cron", "retention"} {
					if v := str(a, k); v != "" {
						sched[k] = v
					}
				}
				// Only "backups"; the route merges env and leaves what it is
				// not given alone, so a schedule change touches nothing else.
				res := call(be, "POST", sbPath(a, "/config"), map[string]any{"backups": sched})
				if res["isError"] == true {
					return res
				}
				// The write answers with the config view, which does not carry
				// the schedule; only the read does. Without this the caller is
				// told nothing about what it just set — including the cron a
				// period was turned into, and when it next fires.
				if got, errText := apiCall(be, "GET", sbPath(a, "/config"), nil, ""); errText == "" {
					return toolText(got)
				}
				return res
			},
		},
		{
			name: "save_snapshot",
			desc: "Save a running sandbox as a named, org-wide snapshot that new sandboxes can be created " +
				"from (pass the name as create_sandbox's template). This is how a slow setup — dependencies " +
				"installed, a database seeded, a build warmed — is paid for once. The capture freezes the " +
				"guest briefly, then the whole memory image is uploaded in the background: the reply is a " +
				"202 with status building, and list_snapshots says when it is ready. Saving the same name " +
				"again adds a version and retires the old one. Minutes, and gigabytes.",
			schema: obj(sandboxArg(map[string]any{
				"name": prop("string", "snapshot name, reused as a template id"),
			}), "sandbox_id", "name"),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "POST", sbPath(a, "/snapshot"), map[string]any{"name": str(a, "name")})
			},
		},
		{
			name: "list_snapshots",
			desc: "The org's named snapshots: name, status (building or ready), runtime, size and the " +
				"resources each was captured with. Check here before creating a sandbox from a snapshot — a " +
				"building one is not bootable yet. Pass name for one snapshot's detail and every version of " +
				"it. A node that does not yet hold a snapshot answers a create with 503 template_fetching " +
				"while it pulls it; retrying succeeds.",
			schema: obj(map[string]any{"name": prop("string", "optional: one snapshot's detail and versions")}),
			run: func(a map[string]any, be Backend) map[string]any {
				p := "/v1/snapshots"
				if name := strings.TrimSpace(str(a, "name")); name != "" {
					p += "/" + url.PathEscape(name)
				}
				return call(be, "GET", p, nil)
			},
		},
		{
			name: "delete_snapshot",
			desc: "Delete a named snapshot and every version of it, org-wide. DESTRUCTIVE and irreversible, " +
				"and it affects everyone in the org: sandboxes already running are unaffected, but nothing " +
				"can be created from the name again. Pass confirm=true.",
			schema: obj(map[string]any{
				"name":    prop("string", "snapshot name (see list_snapshots)"),
				"confirm": prop("boolean", "must be true; without it nothing happens"),
			}, "name", "confirm"),
			run: func(a map[string]any, be Backend) map[string]any {
				name := strings.TrimSpace(str(a, "name"))
				if name == "" {
					return toolError(`delete_snapshot requires a "name" — list_snapshots shows them`)
				}
				if !truthy(a, "confirm") {
					return toolError("delete_snapshot removes every version of " + name +
						" for the whole org and cannot be undone — pass confirm=true to go ahead")
				}
				return call(be, "DELETE", "/v1/snapshots/"+url.PathEscape(name), nil)
			},
		},
		{
			name: "standby_sandbox",
			desc: "Freeze a sandbox in RAM. Cheaper than leaving it running and far quicker to wake than a " +
				"pause, because nothing is written to disk — but it keeps its slot and its memory, so a long " +
				"idle belongs in pause_sandbox instead. resume_sandbox wakes it either way. Use it between " +
				"steps of a session you will come straight back to.",
			schema: obj(sandboxArg(nil), "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "POST", sbPath(a, "/standby"), nil)
			},
		},
		{
			name: "renew_sandbox",
			desc: "Keep a sandbox from expiring. With no timeout_seconds it resets the idle clock — the " +
				"cheap keep-alive to call during a long job so an idle-pause does not land mid-run. With " +
				"timeout_seconds it re-stamps the hard lifetime instead, counted from now; -1 asks for no " +
				"expiry at all, which needs a plan that includes persistent workstations and is refused " +
				"otherwise. A request above the plan's ceiling is silently clamped to it, so read back with " +
				"get_sandbox if the exact expiry matters.",
			schema: obj(sandboxArg(map[string]any{
				"timeout_seconds": prop("integer", "new hard lifetime in seconds; -1 = never expire. Omit to just reset the idle clock"),
			}), "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				if _, set := a["timeout_seconds"]; !set {
					return call(be, "POST", sbPath(a, "/refresh"), nil)
				}
				return call(be, "POST", sbPath(a, "/timeout"), map[string]any{"seconds": num(a, "timeout_seconds")})
			},
		},
		{
			name: "list_files",
			desc: "List a directory in the sandbox workspace as structured entries (name, size, whether it " +
				"is a directory) — use it to find your way around before read_file, rather than parsing ls " +
				"output from run_command. Paths are relative to the workspace root; omit path for the root.",
			schema: obj(sandboxArg(map[string]any{
				"path": prop("string", "directory relative to the workspace root (default the root)"),
			}), "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				p := strings.TrimPrefix(str(a, "path"), "/")
				if p == "" {
					p = "."
				}
				return call(be, "GET", sbPath(a, "/filesystem/list?path="+url.QueryEscape(p)), nil)
			},
		},
		{
			name: "list_secrets",
			desc: "The org's vault entries by name, with whether each is auto-injected. Values of sealed " +
				"entries never come back. Use it to find out what a sandbox can be given without asking a " +
				"human to paste a credential again.",
			schema: obj(map[string]any{}),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "GET", "/v1/secrets", nil)
			},
		},
		{
			name: "put_secret",
			desc: "Store (or overwrite) a named secret in the org vault, so a sandbox can be given it by " +
				"name instead of having the value pasted into env every time. auto_inject puts it into the " +
				"environment of every sandbox the org creates from then on — powerful and easy to regret, so " +
				"leave it off unless the credential really is universal. The value is sealed and cannot be " +
				"read back through the API.",
			schema: obj(map[string]any{
				"name":        prop("string", "env-style name, e.g. OPENAI_API_KEY"),
				"value":       prop("string", "the secret value"),
				"auto_inject": prop("boolean", "inject into every new sandbox in the org (default false)"),
			}, "name", "value"),
			run: func(a map[string]any, be Backend) map[string]any {
				req := map[string]any{"name": str(a, "name"), "value": str(a, "value")}
				if truthy(a, "auto_inject") {
					req["auto_inject"] = true
				}
				return call(be, "POST", "/v1/secrets", req)
			},
		},
		{
			name: "delete_secret",
			desc: "Remove a secret from the org vault. Sandboxes already holding the value keep it; nothing " +
				"new can be given it by name. Nobody can read the value back first, so delete one only when " +
				"you are sure it is either rotated or disposable.",
			schema: obj(map[string]any{"name": prop("string", "secret name (see list_secrets)")}, "name"),
			run: func(a map[string]any, be Backend) map[string]any {
				name := strings.TrimSpace(str(a, "name"))
				if name == "" {
					return toolError(`delete_secret requires a "name" — list_secrets shows them`)
				}
				return call(be, "DELETE", "/v1/secrets/"+url.PathEscape(name), nil)
			},
		},
		{
			name: "create_volume",
			desc: "Create a standalone volume: a disk that outlives the sandbox it is attached to. Attach it " +
				"with create_sandbox's volumes [{id, path}]; destroying that sandbox releases it with its data " +
				"for the next one. One sandbox holds a volume at a time, and a sandbox holding one cannot be " +
				"forked, backed up or moved. Firecracker nodes only.",
			schema: obj(map[string]any{
				"name":    prop("string", "a label for the volume"),
				"size_mb": prop("integer", "256 to 102400"),
			}, "size_mb"),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "POST", "/v1/volumes", map[string]any{"name": str(a, "name"), "size_mb": num(a, "size_mb")})
			},
		},
		{
			name:   "list_volumes",
			desc:   "The org's volumes: id, name, size, and attached_to when a sandbox holds one.",
			schema: obj(map[string]any{}),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "GET", "/v1/volumes", nil)
			},
		},
		{
			name: "delete_volume",
			desc: "Delete a volume and everything on it. Refused while a sandbox holds it; destroy that " +
				"sandbox first. There is no undo.",
			schema: obj(map[string]any{"volume_id": prop("string", "volume id (see list_volumes)")}, "volume_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				id := strings.TrimSpace(str(a, "volume_id"))
				if id == "" {
					return toolError(`delete_volume requires a "volume_id" — list_volumes shows them`)
				}
				return call(be, "DELETE", "/v1/volumes/"+url.PathEscape(id), nil)
			},
		},
		{
			name: "forward_port",
			desc: "Publish a sandbox port on a raw public TCP port, for anything that is not HTTP — a " +
				"database, an SSH daemon, a game server. The reply carries the public host:port to connect " +
				"to. The preview URL is the right answer for web apps; this is the escape hatch below it, " +
				"and it carries no credential of its own, so the sandbox must be visible to the org rather " +
				"than private. Plans cap how many ports one sandbox may publish.",
			schema: obj(sandboxArg(map[string]any{
				"port": prop("integer", "the port inside the sandbox to publish"),
				"udp":  prop("boolean", "also forward UDP (Firecracker only; default TCP only)"),
			}), "sandbox_id", "port"),
			run: func(a map[string]any, be Backend) map[string]any {
				req := map[string]any{"port": num(a, "port")}
				if truthy(a, "udp") {
					req["udp"] = true
				}
				return call(be, "POST", sbPath(a, "/forwards"), req)
			},
		},
		{
			name: "list_port_forwards",
			desc: "The raw public ports this sandbox publishes, each with the address to connect to. Call it " +
				"to recover an endpoint you did not keep, or to see how close the sandbox is to its plan's cap.",
			schema: obj(sandboxArg(nil), "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "GET", sbPath(a, "/forwards"), nil)
			},
		},
		{
			name: "remove_port_forward",
			desc: "Stop publishing a sandbox port. Existing connections through it drop, and the public " +
				"port is released for reuse. The sandbox and the service inside it are untouched.",
			schema: obj(sandboxArg(map[string]any{
				"port": prop("integer", "the sandbox port to stop publishing"),
			}), "sandbox_id", "port"),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "DELETE", sbPath(a, "/forwards/"+strconv.Itoa(num(a, "port"))), nil)
			},
		},
		{
			name: "add_domain",
			desc: "Point a customer hostname at a public sandbox (give sandbox_id, and port if the app is " +
				"not on 8000), or claim a wildcard like *.example.com for the whole org. Nothing routes " +
				"until DNS is in place: the reply carries the CNAME record that has to be created at the " +
				"domain's registrar, which is a step only a human can do — hand the record over and check " +
				"back with list_domains. The sandbox must be publicly reachable, since a custom host carries " +
				"no preview token.",
			schema: obj(map[string]any{
				"host":       prop("string", "hostname, e.g. app.example.com, or *.example.com for an org wildcard"),
				"sandbox_id": prop("string", "the sandbox to serve it (omit for a wildcard)"),
				"port":       prop("integer", "the sandbox port behind the host (default 8000)"),
			}, "host"),
			run: func(a map[string]any, be Backend) map[string]any {
				req := map[string]any{"host": str(a, "host")}
				if v := str(a, "sandbox_id"); v != "" {
					req["sandbox_id"] = v
				}
				if v := num(a, "port"); v != 0 {
					req["port"] = v
				}
				return call(be, "POST", "/v1/domains", req)
			},
		},
		{
			name: "list_domains",
			desc: "The org's custom hostnames with their verification status and the CNAME record each one " +
				"needs. A host stays pending until the platform sees that record in DNS, so this is where " +
				"you find out whether the DNS change actually landed.",
			schema: obj(map[string]any{}),
			run: func(a map[string]any, be Backend) map[string]any {
				return call(be, "GET", "/v1/domains", nil)
			},
		},
		{
			name: "remove_domain",
			desc: "Detach a custom hostname. It stops routing at once and its certificate is no longer " +
				"served, so anything pointing at it breaks; the sandbox keeps its ordinary preview URL. " +
				"The DNS record at the registrar is not yours to remove and stays behind.",
			schema: obj(map[string]any{"host": prop("string", "hostname to detach (see list_domains)")}, "host"),
			run: func(a map[string]any, be Backend) map[string]any {
				host := strings.TrimSpace(str(a, "host"))
				if host == "" {
					return toolError(`remove_domain requires a "host" — list_domains shows them`)
				}
				return call(be, "DELETE", "/v1/domains/"+url.PathEscape(host), nil)
			},
		},
	}
}
