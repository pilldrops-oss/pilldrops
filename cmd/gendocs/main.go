// Command gendocs renders the agent docs (API reference, capability matrix,
// limits) from openapi.json, the launch manifest and the server's constants.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/webhook"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func main() {
	spec := flag.String("spec", "internal/httpapi/openapi.json", "path to openapi.json")
	out := flag.String("out", "docs/agent", "directory to write the generated docs into")
	check := flag.Bool("check", false, "exit non-zero if the checked-in output is stale, writing nothing")
	flag.Parse()

	pages, err := render(*spec)
	if err != nil {
		log.Fatalf("gendocs: %v", err)
	}

	if *check {
		var stale []string
		for name, body := range pages {
			existing, err := os.ReadFile(filepath.Join(*out, name))
			if err != nil || string(existing) != body {
				stale = append(stale, name)
			}
		}
		if len(stale) > 0 {
			sort.Strings(stale)
			fmt.Fprintf(os.Stderr,
				"gendocs: these generated docs are out of date: %s\n  run: make docs\n",
				strings.Join(stale, ", "))
			os.Exit(1)
		}
		fmt.Println("gendocs: generated docs are current")
		return
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("gendocs: %v", err)
	}
	for name, body := range pages {
		p := filepath.Join(*out, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			log.Fatalf("gendocs: write %s: %v", p, err)
		}
		fmt.Printf("gendocs: wrote %s (%d bytes)\n", p, len(body))
	}
}

// render builds every generated page, keyed by file name.
func render(specPath string) (map[string]string, error) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	var spec openAPI
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", specPath, err)
	}
	return map[string]string{
		"api-reference.md":     renderReference(spec),
		"capability-matrix.md": renderCapabilities(),
		"limits.md":            renderLimits(),
	}, nil
}

type openAPI struct {
	Info struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	// Raw per key: a path object also holds non-operation members like shared parameters.
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

type operation struct {
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Deprecated  bool   `json:"deprecated"`
	Parameters  []struct {
		Name        string `json:"name"`
		In          string `json:"in"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
	} `json:"parameters"`
	RequestBody *struct {
		Required bool `json:"required"`
		Content  map[string]struct {
			Schema struct {
				Required   []string `json:"required"`
				Properties map[string]struct {
					Type        string   `json:"type"`
					Description string   `json:"description"`
					Enum        []string `json:"enum"`
				} `json:"properties"`
			} `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Description string `json:"description"`
		Ref         string `json:"$ref"`
	} `json:"responses"`
}

// operationAt decodes one method on one path. A member that fails to decode
// is reported absent rather than fatal.
func operationAt(spec openAPI, path, method string) (operation, bool) {
	raw, ok := spec.Paths[path][method]
	if !ok {
		return operation{}, false
	}
	var op operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return operation{}, false
	}
	return op, true
}

// methodOrder fixes the output order so a regenerate produces the same bytes.
var methodOrder = []string{"get", "post", "patch", "put", "delete"}

// renderReference writes the agent API reference.
func renderReference(spec openAPI) string {
	var b strings.Builder
	b.WriteString("# Agent API reference\n\n")
	b.WriteString(generatedNotice("internal/httpapi/openapi.json"))
	b.WriteString(`
Every endpoint below is scoped to one sandbox whose template declares ` + "`kind: agent`" + `.
Any other sandbox answers **404**, deliberately indistinguishable from a sandbox that does not
exist — so this surface cannot be used to work out which templates an account runs.

Authentication is either an org API key or an **agent access token** (` + "`sfa_…`" + `), which
carries a narrow set of scopes and is bound to one sandbox. See the token guide for which scope
each endpoint needs and why no scope implies another.

`)

	var paths []string
	for p := range spec.Paths {
		if strings.Contains(p, "/agent") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	// Grouped by the segment after /agent/.
	groups := map[string][]string{}
	var order []string
	for _, p := range paths {
		g := groupOf(p)
		if _, seen := groups[g]; !seen {
			order = append(order, g)
		}
		groups[g] = append(groups[g], p)
	}
	sort.Strings(order)

	b.WriteString("## Contents\n\n")
	for _, g := range order {
		fmt.Fprintf(&b, "- [%s](#%s)\n", g, anchor(g))
	}
	b.WriteString("\n")

	for _, g := range order {
		b.WriteString("## " + g + "\n\n")
		for _, p := range groups[g] {
			for _, m := range methodOrder {
				op, ok := operationAt(spec, p, m)
				if !ok {
					continue
				}
				b.WriteString(renderOp(strings.ToUpper(m), p, op))
			}
		}
	}
	return b.String()
}

func renderOp(method, path string, op operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### `%s %s`\n\n", method, path)
	if op.Summary != "" {
		b.WriteString("**" + op.Summary + "**\n\n")
	}
	if op.Deprecated {
		b.WriteString("> **Deprecated.** See the description for the replacement and the sunset date.\n\n")
	}
	if op.Description != "" {
		b.WriteString(op.Description + "\n\n")
	}

	// Skip the path id; every endpoint has it.
	var params []string
	for _, p := range op.Parameters {
		if p.Name == "id" && p.In == "path" {
			continue
		}
		req := ""
		if p.Required {
			req = " *(required)*"
		}
		line := fmt.Sprintf("- `%s` (%s)%s", p.Name, p.In, req)
		if p.Description != "" {
			line += " — " + p.Description
		}
		params = append(params, line)
	}
	if len(params) > 0 {
		b.WriteString("Parameters:\n\n" + strings.Join(params, "\n") + "\n\n")
	}

	if op.RequestBody != nil {
		if c, ok := op.RequestBody.Content["application/json"]; ok && len(c.Schema.Properties) > 0 {
			required := map[string]bool{}
			for _, r := range c.Schema.Required {
				required[r] = true
			}
			var names []string
			for n := range c.Schema.Properties {
				names = append(names, n)
			}
			sort.Strings(names)
			b.WriteString("Body:\n\n")
			for _, n := range names {
				pr := c.Schema.Properties[n]
				line := fmt.Sprintf("- `%s` (%s)", n, pr.Type)
				if required[n] {
					line += " *(required)*"
				}
				if len(pr.Enum) > 0 {
					line += " — one of " + "`" + strings.Join(pr.Enum, "`, `") + "`"
					if pr.Description != "" {
						line += ". " + pr.Description
					}
				} else if pr.Description != "" {
					line += " — " + pr.Description
				}
				b.WriteString(line + "\n")
			}
			b.WriteString("\n")
		}
	}

	var codes []string
	for c := range op.Responses {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	if len(codes) > 0 {
		b.WriteString("Responses:\n\n")
		for _, c := range codes {
			r := op.Responses[c]
			desc := r.Description
			if desc == "" {
				// A $ref to the shared error response.
				desc = "error, with a machine-readable `code` and a message written for a person"
			}
			fmt.Fprintf(&b, "- **%s** — %s\n", c, desc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// groupOf names the section a path belongs to.
func groupOf(path string) string {
	rest := path
	if i := strings.Index(path, "/agent"); i >= 0 {
		rest = strings.TrimPrefix(path[i+len("/agent"):], "/")
	}
	seg := rest
	if i := strings.Index(seg, "/"); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case "", "status":
		return "Session"
	case "turns", "queue":
		return "Turns and the queue"
	case "events":
		return "Event stream"
	case "approvals", "policy":
		return "Approvals (Guided mode)"
	case "tokens", "share", "claim":
		return "Access tokens and sharing"
	case "webhooks":
		return "Webhooks"
	case "responses":
		return "OpenAI-compatible adapter"
	case "budget":
		return "Spend"
	case "connect", "isolation":
		return "Connect and isolation"
	case "models", "threads":
		return "Models and threads"
	default:
		// Path segments are ASCII, so byte-wise capitalisation is correct.
		return strings.ToUpper(seg[:1]) + seg[1:]
	}
}

func anchor(title string) string {
	a := strings.ToLower(title)
	a = strings.NewReplacer(" ", "-", "(", "", ")", "", ",", "").Replace(a)
	return a
}

// renderCapabilities writes the per-agent matrix from the launch manifest,
// which is what the runtime reads.
func renderCapabilities() string {
	var b strings.Builder
	b.WriteString("# Agent capability matrix\n\n")
	b.WriteString(generatedNotice("the launch manifest (internal/templates)"))
	b.WriteString(`
Every column is a capability that was **verified against a live CLI** at the pinned version in
the manifest, not read from a vendor's documentation. That distinction is the point of the
table: several of these agents advertise features their CLI did not actually deliver when we
ran it, and a platform that repeated the claim would be lying on their behalf.

A blank cell is therefore not a gap in our research — it is a capability the agent did not
demonstrate, and the consequences of each are spelled out below the table.

| Agent | Follow-up | Approval gate | Tool timeline | Model choice |
|---|---|---|---|---|
`)
	var ids []string
	byID := map[string]templates.Template{}
	for _, t := range templates.Catalog() {
		if t.Kind != templates.KindAgent || t.Agent == nil {
			continue
		}
		ids = append(ids, t.ID)
		byID[t.ID] = t
	}
	sort.Strings(ids)

	mark := func(ok bool) string {
		if ok {
			return "yes"
		}
		return "—"
	}
	for _, id := range ids {
		t := byID[id]
		a := t.Agent
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
			id,
			mark(a.ContinueCmd != "" || a.ResumeCmd != ""),
			mark(a.PermissionTool != ""),
			mark(a.StreamFormat != ""),
			mark(a.ModelEnv != ""))
	}

	b.WriteString(`
### What a blank cell costs you

**Follow-up** — the agent's CLI has no resume flag we could verify, so every message starts a
fresh conversation. It will not recall what you said a minute ago. The status endpoint reports
this as ` + "`remembers: false`" + ` with a note, so a client can say so rather than presenting a
thread that silently forgets.

**Approval gate** — the CLI has no hook we could route tool decisions through, so **Guided mode
is not offered** for this agent. It is not disabled out of caution: an agent that accepted the
flag and ignored it would run completely ungated while the interface claimed every action was
reviewed, which is worse than having no approvals feature at all. Supervised and Autonomous
still work.

**Tool timeline** — no structured event stream, so the transcript is the agent's raw output
rather than a list of tool calls with durations. Everything still runs; you just see it as text.

**Model choice** — the agent takes its model from its own config file rather than an environment
variable, so the Model tab writes that file instead of setting a variable.
`)
	return b.String()
}

// defaultRetentionDays mirrors the server's default for AGENT_EVENT_RETENTION_DAYS.
// A constant rather than the env, which is unset on dev and build machines; a test pins it.
const defaultRetentionDays = 30

// renderLimits publishes the limits from the constants that enforce them.
func renderLimits() string {
	var b strings.Builder
	b.WriteString("# Agent limits, retention and rate limits\n\n")
	b.WriteString(generatedNotice("the constants the server enforces"))
	b.WriteString(`
## Event retention

| What | Kept for | Notes |
|---|---|---|
| Journal events in the sandbox | Until the sandbox is destroyed | The journal lives on the workstation's disk, which is in its snapshot — so pause and resume keep it. |
| Mirrored events in the control plane | **` + strconv.Itoa(defaultRetentionDays) + ` days** (` + "`AGENT_EVENT_RETENTION_DAYS`" + `) | Set per deployment. ` + "`0`" + ` disables pruning entirely rather than meaning "delete everything" — the reading that would lose a customer's history to a typo. |
| Mirrored **turns** in the control plane | Not pruned | Turns are the record of what the agent did and what it cost. Events are the detail of how; turns are the fact that it happened. |

Retention applies to the control plane's copy only. Reading history from a **running**
workstation always goes to the sandbox itself, so a short retention window never shortens what a
live machine can tell you.

## Output truncation

A single turn's output is capped in the journal. When the cap is hit, a
` + "`turn.output_truncated`" + ` event is emitted and clients must surface it — a transcript that
simply ends looks complete, and that is the one failure mode worse than showing less.

## Webhook delivery

| Limit | Value | Why |
|---|---|---|
| Attempts per delivery | ` + strconv.Itoa(webhook.MaxAttempts) + ` | Full-jitter exponential backoff between them. |
| Consecutive failures before auto-disable | ` + strconv.Itoa(webhook.AutoDisableAfter) + ` | Consecutive, not cumulative: 20 failures over a year is a flaky receiver, 20 in a row is an endpoint that has gone away. |
| One attempt's timeout | ` + webhook.DeliveryTimeout.String() + ` | A receiver that cannot answer in that time would otherwise become our queue depth. |
| Signature timestamp tolerance | ` + webhook.MaxSkew.String() + ` | Reject anything outside it. The signature covers the timestamp, so a receiver that does not check freshness gains nothing from the signing. |
| Test sends | 5 per minute per hook | This endpoint makes our server call an address you chose; without a budget it is an amplifier. |

A ` + "`4xx`" + ` other than ` + "`408`" + ` or ` + "`429`" + ` stops retrying immediately: the
receiver understood the request and refused it, so repeating it just delivers the same refusal
five more times.

**Not every event is deliverable.** ` + "`turn.output`" + ` is refused — it is the agent's raw
output stream, flushed several times a second, and forwarding it would flood an endpoint that
signed up for notifications. Subscribe to ` + "`turn.completed`" + ` and read the transcript. The
deliverable set is returned by the webhook list endpoint, from the same predicate the sender
enforces, so a client cannot offer an event that will never arrive.

## Queue and approvals

| Limit | Value | Why |
|---|---|---|
| Approval timeout | 15 minutes | **A timeout is a denial.** Silence is never taken as consent. |
| Pending approvals per sandbox | 32 | A bounded queue: an agent that parks thousands of calls would otherwise exhaust memory in the sandbox, and no human is going to review a thousand prompts. |
| Resolved approvals kept in memory | 200 | The durable record is in the control plane; this is the recent window a UI renders. |
`)
	return b.String()
}

func generatedNotice(source string) string {
	return "> **Generated file — do not edit.** Rendered from " + source + " by\n" +
		"> `cmd/gendocs`. Run `make docs` after changing the source; a test fails if this\n" +
		"> file is stale. Editing it by hand means the next regenerate silently discards\n" +
		"> your words.\n"
}
