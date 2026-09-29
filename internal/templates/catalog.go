// Package templates is the source of truth for the built-in sandbox templates,
// backing the HTTP catalog, create-time validation and the web UI picker.
package templates

import (
	"context"
	"errors"
	"fmt"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// TemplateEnv declares an environment variable a template understands, for the launch UI.
type TemplateEnv struct {
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	Required bool   `json:"required,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
}

// Template describes a sandbox image and its recommended resources. Aliases
// are accepted on create but never listed, so legacy ids do not appear twice.
type Template struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Language    string   `json:"language"`
	Icon        string   `json:"icon"`
	Tags        []string `json:"tags"`
	CPU         int      `json:"cpu"`
	MemoryMB    int      `json:"memory_mb"`
	StorageMB   int      `json:"storage_mb"`
	Visibility  string   `json:"visibility"`
	// Port is where the template's built-in app listens; creates publish it as
	// the sandbox app port so preview links route there instead of 8000.
	Port int `json:"port,omitempty"`

	// Launch UI metadata, all optional; a custom image's template.json can override.
	Preinstalled    []string      `json:"preinstalled,omitempty"`
	StartSuggestion string        `json:"start_suggestion,omitempty"`
	SetupSuggestion []string      `json:"setup_suggestion,omitempty"`
	HasServer       bool          `json:"has_server,omitempty"`
	ServerDesc      string        `json:"server_desc,omitempty"`
	DefaultPort     int           `json:"default_port,omitempty"`
	DocsURL         string        `json:"docs_url,omitempty"`
	Env             []TemplateEnv `json:"env,omitempty"`

	// Launch manifest, populated for built-ins by manifest.go; a disk template.json can override.
	Kind         string        `json:"kind,omitempty"`
	PrimaryInput *PrimaryInput `json:"primary_input,omitempty"`
	Outcome      string        `json:"outcome,omitempty"`
	Agent        *AgentMeta    `json:"agent,omitempty"`

	// Image and Ports mark an image-backed service template booted from a
	// published image with its own entrypoint. Platform-defined, so exempt from
	// the node's image allowlist unlike a caller-supplied image.
	Image string `json:"image,omitempty"`
	Ports []int  `json:"ports,omitempty"`

	Aliases []string `json:"-"`
}

const defaultID = "base"

// catalog is the built-in templates in display order.
var catalog = []Template{
	{
		ID: "base", Name: "Base",
		Description: "Minimal Debian: bash, curl, git, ca-certificates",
		Language:    "shell", Icon: "sh", Tags: []string{"minimal"},
		CPU: 1, MemoryMB: 512, StorageMB: 1024, Visibility: "public",
		Preinstalled: []string{"bash", "curl", "git"},
		DefaultPort:  8000,
	},
	{
		// Firecracker only, and pinned there: a nested daemon needs a kernel of
		// its own (templateRuntime, internal/orchestrator/nodes.go; the
		// NestedDocker refusal in internal/httpapi/sandboxes.go).
		ID: "docker", Name: "Docker",
		Description: "A VM with Docker Engine, Compose and Buildx — the daemon is already running, so `docker run -p 8000:80` goes live on the preview URL",
		Language:    "shell", Icon: "dk",
		Tags: []string{"docker", "compose", "buildx", "containers"},
		CPU:  2, MemoryMB: 2048, StorageMB: 20480, Visibility: "public",
		Port:            8000,
		Preinstalled:    []string{"docker", "compose", "buildx"},
		StartSuggestion: "docker run -d -p $PORT:80 nginx:alpine",
		DefaultPort:     8000,
	},
	{
		ID: "code-interpreter", Name: "Code Interpreter",
		Description: "Python 3.11 + Node 20 + numpy, pandas, matplotlib, scipy, scikit-learn",
		Language:    "python", Icon: "py",
		Tags: []string{"python", "node", "jupyter", "data-science"},
		CPU:  2, MemoryMB: 1024, StorageMB: 4096, Visibility: "public",
		Preinstalled:    []string{"python3.11", "node20", "npm", "numpy", "pandas", "matplotlib", "scipy", "scikit-learn"},
		StartSuggestion: "python3 -m http.server $PORT",
		DefaultPort:     8000,
		Aliases:         []string{"python-3.11", "python-data-science", "code-interpreter-v1"},
	},
	{
		// Shares the code-interpreter image; see canonicalTemplate.
		ID: "research", Name: "Deep Research",
		Description: "Research workspace: Python + requests/pandas ready — describe your question with ✨ AI Configure (it writes the plan), findings served on the preview URL.",
		Language:    "python", Icon: "🔎", Tags: []string{"research", "python", "report", "use-case"},
		CPU: 2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Preinstalled:    []string{"python3.11", "requests", "pandas", "matplotlib", "node20"},
		StartSuggestion: "python3 -m http.server $PORT",
		DefaultPort:     8000,
		Aliases:         []string{"deep-research"},
	},
	{
		// Shares the code-interpreter image; see canonicalTemplate.
		ID: "web-scraper", Name: "Web Scraper",
		Description: "Scrape a page in seconds: the Python starter fetches, extracts and serves the results — point ✨ AI Configure at your target site and it writes the scraper.",
		Language:    "python", Icon: "🕷", Tags: []string{"scraping", "python", "use-case"},
		CPU: 2, MemoryMB: 1024, StorageMB: 4096, Visibility: "public",
		Preinstalled:    []string{"python3.11", "requests", "pandas"},
		StartSuggestion: "python3 scrape.py",
		SetupSuggestion: []string{"pip install -q beautifulsoup4"},
		DefaultPort:     8000,
		Aliases:         []string{"scraper"},
	},
	{
		// Shares the node image. Launches public by default so external services can reach it.
		ID: "webhook-inbox", Name: "Webhook Inbox",
		Description: "A live URL that catches webhooks: every request is logged to a page that updates in real time. Paste the public URL into Stripe, GitHub, or anything that POSTs.",
		Language:    "node", Icon: "📥", Tags: []string{"webhooks", "testing", "use-case"},
		CPU: 1, MemoryMB: 512, StorageMB: 2048, Visibility: "public",
		Preinstalled:    []string{"node20"},
		StartSuggestion: "node inbox.js",
		DefaultPort:     8000,
		Aliases:         []string{"webhook-tester"},
	},
	{
		ID: "fastapi", Name: "FastAPI",
		Description: "FastAPI + uvicorn, SQLAlchemy, Pydantic, httpx pre-installed on Python 3.11",
		Language:    "python", Icon: "🚀", Tags: []string{"python", "api", "fastapi"},
		CPU: 2, MemoryMB: 1024, StorageMB: 4096, Visibility: "public",
		Preinstalled:    []string{"python3.11", "fastapi", "uvicorn", "sqlalchemy", "httpx"},
		StartSuggestion: "uvicorn main:app --host 0.0.0.0 --port $PORT",
		// Load-bearing on docker nodes, which boot bare python:3.11-slim for this
		// id; drop once an enriched fastapi image ships.
		SetupSuggestion: []string{"pip install fastapi uvicorn"},
		DefaultPort:     8000,
	},
	{
		ID: "browser", Name: "Browser",
		Description: "Headless Chromium you watch and control in the browser — lightweight computer-use, no VNC or desktop.",
		Language:    "node", Icon: "🌐",
		Tags: []string{"browser", "computer-use", "gui"},
		CPU:  2, MemoryMB: 2048, StorageMB: 5120, Visibility: "public",
		Port:         8000,
		Preinstalled: []string{"chromium"},
		HasServer:    true,
		ServerDesc:   "Live Chromium (CDP screencast) on the preview URL — click, type, navigate",
		DefaultPort:  8000,
	},
	{
		ID: "desktop", Name: "Desktop",
		Description: "Full Linux desktop (fluxbox) in your browser — run any GUI app; xdotool + scrot for computer-use agents. Want just a browser? Use the Browser template.",
		Language:    "shell", Icon: "🖥",
		Tags: []string{"gui", "computer-use", "vnc"},
		CPU:  2, MemoryMB: 2048, StorageMB: 5120, Visibility: "public",
		// The viewer is envd's, on the platform desktop port, so this
		// template's preview link goes there rather than to an app port
		// nothing binds. Every other template reaches the same viewer on the
		// same port with PD_DESKTOP=1; this one bakes the flag.
		Port:         rt.DesktopPort,
		Preinstalled: []string{"fluxbox", "chromium", "xterm", "xdotool", "scrot"},
		HasServer:    true,
		ServerDesc:   "noVNC desktop streamed on the preview URL",
		DefaultPort:  rt.DesktopPort,
	},
	{
		ID: "android", Name: "Android Emulator",
		Description: "Android 14 emulator (KVM-accelerated) with a browser-viewable screen + Appium & adb inside — mobile E2E testing and app agents",
		Language:    "android", Icon: "🤖",
		Tags: []string{"mobile", "emulator", "e2e", "appium", "computer-use"},
		CPU:  4, MemoryMB: 8192, StorageMB: 16384, Visibility: "public",
		Port:         6080,
		Preinstalled: []string{"android14", "appium", "adb"},
		HasServer:    true,
		ServerDesc:   "Android screen (noVNC) on the preview URL",
		DefaultPort:  6080,
		Aliases:      []string{"android-emulator", "android-14"},
	},
	{
		ID: "android-headless", Name: "Android Emulator (headless CI)",
		Description: "Headless Android 14 emulator + Appium for CI/E2E — no screen streaming (lower overhead); the preview URL is the Appium WebDriver endpoint",
		Language:    "android", Icon: "🤖",
		Tags: []string{"mobile", "emulator", "e2e", "appium", "ci", "headless"},
		CPU:  4, MemoryMB: 8192, StorageMB: 16384, Visibility: "public",
		Port:         4723,
		Preinstalled: []string{"android14", "appium"},
		HasServer:    true,
		ServerDesc:   "Appium WebDriver endpoint on the preview URL",
		DefaultPort:  4723,
		Aliases:      []string{"android-ci", "android-appium"},
	},
	{
		ID: "mobile-test", Name: "Mobile Test Session",
		Description: "Zero-setup mobile E2E: drop your test script in, it runs on a headless Android 14 emulator with automatic screen recording — the sandbox URL shows live progress, then the full report (video, screenshot, log, artifacts). One command: pilldrops test ./suite.sh",
		Language:    "android", Icon: "▶",
		Tags: []string{"mobile", "e2e", "testing", "recording", "report", "ci"},
		CPU:  4, MemoryMB: 8192, StorageMB: 16384, Visibility: "public",
		Port:         8765,
		Preinstalled: []string{"android14", "recording"},
		HasServer:    true,
		ServerDesc:   "Live test progress + report on the preview URL",
		DefaultPort:  8765,
		Aliases:      []string{"android-test", "mobile-e2e"},
	},
	{
		// Contains no OpenClaw; the id is kept because sandboxes re-boot from
		// their stored template id. Real OpenClaw is "openclaw-agent".
		ID: "openclaw", Name: "Browser Agent (Python)",
		Description: "Browser automation in Python: Chromium + playwright, selenium, beautifulsoup4, flask pre-installed",
		Language:    "python", Icon: "🦾", Tags: []string{"python", "browser", "automation"},
		Aliases: []string{"browser-agent"},
		CPU:     4, MemoryMB: 4096, StorageMB: 5120, Visibility: "public",
		Preinstalled: []string{"python3.11", "chromium", "playwright", "selenium"},
		DefaultPort:  8000,
	},
	{
		ID: "playwright", Name: "Playwright (E2E testing)",
		Description: "Pre-baked Playwright + Chromium (browsers & OS deps included) — write E2E tests, run them, serve the HTML report. Zero install.",
		Language:    "node", Icon: "pw",
		Tags: []string{"browser", "testing", "e2e", "recording"},
		CPU:  2, MemoryMB: 2048, StorageMB: 5120, Visibility: "public",
		Preinstalled:    []string{"chromium", "playwright"},
		StartSuggestion: "npx playwright test",
		DefaultPort:     8000,
		Aliases:         []string{"playwright-test", "e2e"},
	},
	{
		ID: "security", Name: "Security review",
		Description: "Scan code you own: osv-scanner finds known-vulnerable dependencies, gitleaks finds committed secrets, and pdscan checks a URL's response headers. GREEN/RED verdict + report on the preview URL. No active probing.",
		Language:    "python", Icon: "⛨",
		Tags: []string{"security", "testing", "dependencies", "secrets", "report"},
		CPU:  2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Port:         8000, // report server (start.sh) — preview URL = the report
		Preinstalled: []string{"osv-scanner", "gitleaks", "python3"},
		HasServer:    true,
		ServerDesc:   "Security report on the preview URL",
		DefaultPort:  8000,
		Aliases:      []string{"scan", "sec"},
	},
	{
		ID: "loadtest", Name: "Load test (k6)",
		Description: "Pre-baked k6 load generator: pdload <url> ramps virtual users, checks thresholds and publishes a GREEN/RED report on the preview URL. Test only targets you own.",
		Language:    "javascript", Icon: "▮",
		Tags: []string{"testing", "load", "performance", "k6", "report"},
		CPU:  2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Port:         8000, // report server (start.sh) — preview URL = the report
		Preinstalled: []string{"k6", "python3"},
		HasServer:    true,
		ServerDesc:   "Load-test report on the preview URL",
		DefaultPort:  8000,
		Aliases:      []string{"k6", "stress-test"},
	},
	{
		ID: "claude", Name: "Claude Code",
		Description: "Node 20 + Claude Code CLI (@anthropic-ai/claude-code)",
		Language:    "node", Icon: "cc", Tags: []string{"agent", "coding-cli"},
		CPU: 1, MemoryMB: 1024, StorageMB: 2048, Visibility: "public",
		Preinstalled: []string{"node20", "claude-code"},
		DefaultPort:  8000,
	},
	{
		ID: "codex", Name: "Codex",
		Description: "Node 20 + OpenAI Codex CLI (@openai/codex)",
		Language:    "node", Icon: "cx", Tags: []string{"agent", "coding-cli"},
		CPU: 1, MemoryMB: 1024, StorageMB: 2048, Visibility: "public",
		Preinstalled: []string{"node20", "codex"},
		DefaultPort:  8000,
	},
	{
		ID: "grok", Name: "Grok",
		Description: "Node 20 + Grok CLI",
		Language:    "node", Icon: "gk", Tags: []string{"agent", "coding-cli"},
		CPU: 1, MemoryMB: 1024, StorageMB: 2048, Visibility: "public",
		Preinstalled: []string{"node20", "grok"},
		DefaultPort:  8000,
	},
	{
		ID: "opencode", Name: "OpenCode",
		Description: "Node 20 + OpenCode (opencode-ai)",
		Language:    "node", Icon: "oc", Tags: []string{"agent", "coding-cli"},
		CPU: 1, MemoryMB: 1024, StorageMB: 2048, Visibility: "public",
		Preinstalled: []string{"node20", "opencode"},
		DefaultPort:  8000,
	},
	{
		ID: "openclaw-agent", Name: "OpenClaw",
		Description: "Node 24 + OpenClaw — multi-channel AI gateway agent (github.com/openclaw/openclaw); bring any provider key",
		Language:    "node", Icon: "ow", Tags: []string{"agent", "coding-cli", "channels"},
		// Heavier than the thin CLI agents: carries its own gateway, channel bridges and memory index.
		CPU: 2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Preinstalled: []string{"node24", "openclaw", "git"},
		DefaultPort:  8000,
	},
	{
		ID: "hermes", Name: "Hermes",
		Description: "Node 24 + Hermes Agent — self-improving, tool-calling, provider-agnostic (bring any provider key)",
		Language:    "node", Icon: "hm", Tags: []string{"agent", "coding-cli"},
		// Heavier than the thin CLI agents: carries its own tooling, skills and memory index.
		CPU: 2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Preinstalled: []string{"node24", "hermes", "git"},
		DefaultPort:  8000,
	},
	{
		ID: "amp", Name: "Amp",
		Description: "Node 20 + Amp CLI (@sourcegraph/amp)",
		Language:    "node", Icon: "⚡", Tags: []string{"agent", "coding-cli"},
		CPU: 1, MemoryMB: 1024, StorageMB: 2048, Visibility: "public",
		Preinstalled: []string{"node20", "amp"},
		DefaultPort:  8000,
	},
	{
		ID: "node", Name: "Node + TypeScript",
		Description: "Node 20 + typescript, tsx, ts-node",
		Language:    "node", Icon: "ts", Tags: []string{"node", "typescript"},
		CPU: 1, MemoryMB: 512, StorageMB: 2048, Visibility: "public",
		Preinstalled:    []string{"node20", "typescript", "tsx"},
		StartSuggestion: "node server.js",
		DefaultPort:     8000,
		Aliases:         []string{"node-20"},
	},
	{
		ID: "nextjs", Name: "Next.js",
		Description: "Next.js starter app pre-baked at /root/app — npm run dev serves instantly",
		Language:    "node", Icon: "▲", Tags: []string{"node", "react", "nextjs"},
		CPU: 2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Preinstalled:    []string{"node20", "nextjs starter (/root/app)"},
		StartSuggestion: "cd /root/app && npm run dev",
		// Not 3000: runtimes inject PORT=8000 and the starter binds ${PORT:-3000}.
		DefaultPort: 8000,
	},
	{
		ID: "go", Name: "Go",
		Description: "Go 1.22 toolchain + git",
		Language:    "go", Icon: "go", Tags: []string{"go"},
		CPU: 1, MemoryMB: 1024, StorageMB: 4096, Visibility: "public",
		Preinstalled:    []string{"go1.22", "git"},
		StartSuggestion: "go run .",
		DefaultPort:     8000,
		Aliases:         []string{"go-1.22"},
	},
	{
		ID: "rust", Name: "Rust",
		Description: "Rust 1.77 toolchain (cargo, rustc) + git",
		Language:    "rust", Icon: "🦀", Tags: []string{"rust", "cargo"},
		CPU: 2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Preinstalled:    []string{"cargo", "rustc", "git"},
		StartSuggestion: "cargo run",
		DefaultPort:     8000,
		Aliases:         []string{"rust-1.77"},
	},

	// Image-backed services: Preinstalled names the service version, not tools on a PATH.
	{
		ID: "postgres", Name: "Postgres",
		Description: "PostgreSQL 16 relational database.",
		Language:    "sql", Icon: "🐘", Tags: []string{"database", "sql", "postgres"},
		CPU: 2, MemoryMB: 1024, StorageMB: 4096, Visibility: "public",
		Image: "postgres:16", Ports: []int{5432},
		// The official image refuses to start without POSTGRES_PASSWORD.
		Env: []TemplateEnv{
			{Name: "POSTGRES_PASSWORD", Required: true, Secret: true},
			{Name: "POSTGRES_DB", Value: "app"},
		},
		Preinstalled: []string{"postgresql-16"},
		DefaultPort:  5432,
	},
	{
		ID: "redis", Name: "Redis",
		Description: "Redis 7 in-memory data store / cache.",
		Language:    "shell", Icon: "🔴", Tags: []string{"database", "cache", "redis"},
		CPU: 2, MemoryMB: 512, StorageMB: 2048, Visibility: "public",
		Image: "redis:7", Ports: []int{6379},
		Preinstalled: []string{"redis-7"},
		DefaultPort:  6379,
	},
	{
		ID: "mysql", Name: "MySQL",
		Description: "MySQL 8 relational database.",
		Language:    "sql", Icon: "🐬", Tags: []string{"database", "sql", "mysql"},
		CPU: 2, MemoryMB: 1024, StorageMB: 4096, Visibility: "public",
		Image: "mysql:8", Ports: []int{3306},
		Env: []TemplateEnv{
			{Name: "MYSQL_ROOT_PASSWORD", Required: true, Secret: true},
		},
		Preinstalled: []string{"mysql-8"},
		DefaultPort:  3306,
	},
	{
		ID: "mongodb", Name: "MongoDB",
		Description: "MongoDB 7 document database.",
		Language:    "shell", Icon: "🍃", Tags: []string{"database", "nosql", "mongodb"},
		CPU: 2, MemoryMB: 1024, StorageMB: 4096, Visibility: "public",
		Image: "mongo:7", Ports: []int{27017},
		Preinstalled: []string{"mongodb-7"},
		DefaultPort:  27017,
	},
	{
		ID: "kafka", Name: "Kafka",
		Description: "Apache Kafka 3.8 event streaming platform.",
		Language:    "shell", Icon: "📨", Tags: []string{"queue", "streaming", "kafka"},
		CPU: 2, MemoryMB: 2048, StorageMB: 4096, Visibility: "public",
		Image: "apache/kafka:3.8.0", Ports: []int{9092},
		Preinstalled: []string{"kafka-3.8"},
		DefaultPort:  9092,
	},
}

// Catalog returns built-ins in display order, then disk-only templates. A disk
// manifest for a built-in id overrides it in place. Callers must not mutate the slices.
func Catalog() []Template {
	disk := discovered()
	byID := make(map[string]Template, len(disk))
	for _, t := range disk {
		byID[t.ID] = t
	}
	out := make([]Template, 0, len(catalog)+len(disk))
	for _, t := range catalog {
		if d, ok := byID[t.ID]; ok {
			t = d
			delete(byID, t.ID)
		}
		out = append(out, t)
	}
	for _, t := range disk {
		if _, extra := byID[t.ID]; extra {
			out = append(out, t)
		}
	}
	return out
}

// Resolve returns the template for a canonical id or alias, disk first so a
// published manifest wins over the compiled-in entry. It never fetches; use
// ResolveFetch on the create path, so read paths never trigger a download.
func Resolve(id string) (Template, bool) {
	if t, ok := fromDisk(id); ok {
		return t, true
	}
	return builtin(id)
}

// ResolveFetch is Resolve for the create path: disk, then the artifact store
// (which fetches into the image directory so disk is re-read), then the
// compiled-in catalog. The store sits above the catalog because a catalog hit
// would end resolution before the fetch that makes the id bootable. Errors are
// ErrUnknownTemplate (caller's 400) or ErrFetchFailed (operator problem).
func ResolveFetch(ctx context.Context, id string) (Template, error) {
	t, _, err := ResolveFetchTimed(ctx, id)
	return t, err
}

// FetchStat reports what the artifact-store tier did, so a slow create can be
// attributed to the fetch rather than the boot.
type FetchStat struct {
	// Fetched is true only when the store tier actually materialised the template.
	Fetched bool
	// Duration is zero when the store tier was not consulted.
	Duration time.Duration
}

// ResolveFetchTimed is ResolveFetch with the store tier's cost reported.
// The disk hits below are conditional on heldIsStale: an id names a template,
// not a version of one, so "this node has a directory called X" is not the same
// fact as "this node has the X that is published now". Without the check a node
// that fetched v1 serves it forever and the create still reports success.
func ResolveFetchTimed(ctx context.Context, id string) (Template, FetchStat, error) {
	if t, ok := fromDisk(id); ok && !heldIsStale(ctx, id) {
		return t, FetchStat{}, nil
	}

	// The artifact index is keyed by canonical id, so canonicalise aliases
	// before fetching, and re-check disk so a held template is not re-fetched.
	fetchID := id
	if t, ok := builtin(id); ok && t.ID != id {
		fetchID = t.ID
		if dt, ok := fromDisk(fetchID); ok && !heldIsStale(ctx, fetchID) {
			return dt, FetchStat{}, nil
		}
	}

	started := time.Now()
	err := fetchIntoDir(ctx, fetchID)
	stat := FetchStat{Fetched: err == nil, Duration: time.Since(started)}
	switch {
	case err == nil:
		if t, ok := fromDisk(fetchID); ok {
			return t, stat, nil
		}
		return Template{}, stat, fmt.Errorf("%w: %s was fetched but the image directory does not "+
			"discover it — the fetched layout does not match what the scan reads", ErrFetchFailed, fetchID)
	case errors.Is(err, ErrFetchInProgress):
		// Not wrapped in ErrFetchFailed: nothing failed, and the caller has to
		// be able to tell "retry in a moment" from "this node is broken".
		return Template{}, stat, err
	case errors.Is(err, errNoStoreTier), errors.Is(err, ErrNotInStore):
		// No fetch was performed, so report no duration either.
		stat = FetchStat{}
	default:
		return Template{}, stat, fmt.Errorf("%w: %s: %w", ErrFetchFailed, fetchID, err)
	}

	if t, ok := builtin(id); ok {
		return t, stat, nil
	}
	return Template{}, stat, fmt.Errorf("%w: %s", ErrUnknownTemplate, id)
}

// fromDisk looks up one disk-discovered template by exact id; aliases are not resolved.
func fromDisk(id string) (Template, bool) {
	for _, t := range discovered() {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// HasDesktopStack reports whether this template's image holds the on-demand
// screen stack (Xvfb, fluxbox, x11vnc, noVNC), which is one half of a sandbox's
// has_desktop — the other half is whether the runtime can host an X server.
//
// The platform's own bake adds the stack to every image it builds
// (scripts/build-template.sh for Firecracker, templateImage for Docker), so the
// rule is "this is a template the platform ships and therefore bakes", declared
// here once rather than as an entry repeated in 25 Preinstalled lists.
//
// builtin, NOT Resolve. Resolve also answers for anything published on this
// node — any directory holding a rootfs.squashfs is a resolvable template,
// manifest or not — so it returned true for a caller's own published image,
// which the spec says must read false, and for a template deliberately baked
// with PD_NO_DESKTOP=1. Measured on a node: a `base-nodesktop` image built with
// the opt-out reported has_desktop=true. builtin answers only for the
// compiled-in catalogue and its aliases, which is exactly the set the platform
// bakes the layer into.
//
// Service templates are false because they run a vendor image with its own
// entrypoint and the bake never touches them. A catalogue template deliberately
// built with PD_NO_DESKTOP=1 still reads true — that is the opt-out's known
// cost, and scripts/verify-templates.sh is what catches it, since it probes the
// binaries on every template rather than trusting this declaration.
func HasDesktopStack(id string) bool {
	t, ok := builtin(id)
	return ok && t.Image == ""
}

// DesktopRequested reports whether this sandbox asked for the screen, which is
// the half of has_desktop that used to be missing.
//
// envd starts Xvfb, fluxbox, x11vnc and noVNC only when PD_DESKTOP=1 is in the
// session env (cmd/envd/desktop.go). Nothing sets it on `base`, so `base`
// reported has_desktop=true, `sandbox open` handed out a link to port 6080, and
// nothing was ever listening there. Read off the same env the guest gets, so
// the answer and the guest's own decision cannot drift.
func DesktopRequested(env []string, template string) bool {
	for _, e := range env {
		if e == "PD_DESKTOP=1" {
			return true
		}
	}
	return DesktopBaked(template)
}

// DesktopBaked reports a template whose image bakes PD_DESKTOP=1, which is every
// template whose own preview port IS the desktop port.
//
// It is all a response tier can answer with. The create env is not kept past the
// boot — Sandbox.Env holds the DEPLOY start env, which SetRunSpec overwrites —
// so a sandbox created with PD_DESKTOP=1 on an ordinary template reads false
// here and gets no desktop_url. That is a deliberate under-claim: the screen is
// still reachable with `sandbox open --port 6080`, whereas the over-claim this
// replaces put a dead 6080 link on EVERY sandbox and made it the first one
// clients picked up. Persist the create-time answer if the under-claim starts
// costing more than a flag does.
func DesktopBaked(template string) bool {
	t, ok := Resolve(template)
	return ok && t.Port == rt.DesktopPort
}
