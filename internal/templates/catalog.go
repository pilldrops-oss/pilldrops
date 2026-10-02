// Package templates is the source of truth for the built-in sandbox templates,
// backing the HTTP catalog, create-time validation and the web UI picker.
package templates

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// Internal reports whether id is a node-internal template: a snapshot capture
// (store.SnapshotTemplateID, a move's, a rehome's). Its id names its org and
// sandbox, so no tenant catalogue lists it; resume and restore resolve it by id.
func Internal(id string) bool { return strings.HasPrefix(id, "snap-") }

// Listed is Catalog without the internal templates: what a tenant is shown.
func Listed() []Template {
	cat := Catalog()
	out := make([]Template, 0, len(cat))
	for _, t := range cat {
		if !Internal(t.ID) {
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
