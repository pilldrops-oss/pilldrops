// Package docker is a Runtime that backs each sandbox with a Docker container
// built from the template's image. It is the stand-in for Firecracker on hosts
// without KVM.
package docker

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// sessionHarness is the mobile-test session UI and watcher injectHarness copies in.
//
//go:embed session_harness
var sessionHarness embed.FS

// appPort is the fixed in-container port apps bind via $PORT, published to a
// random host port at boot.
const appPort = 8000

// terminalPort is where envd's web terminal listens in the container.
const terminalPort = rt.TerminalPort

// desktopPort is where envd's noVNC viewer listens when the create asked for a
// screen. Published only then: a published port costs a docker-proxy per
// container, and a desktop nobody asked for is what this change exists to stop.
const desktopPort = rt.DesktopPort

// wantsDesktop reports whether this create asked envd to start the screen
// stack. Read off the same env the guest gets, so the host's publish decision
// and the guest's start decision cannot drift.
func wantsDesktop(t rt.Template) bool {
	return templates.DesktopRequested(t.Env, t.Name)
}

// containerWorkdir is the sandbox root inside every container.
const containerWorkdir = "/workspace"

// The unprivileged identity agentd runs under: the `node` user every official
// node image ships, so no useradd is needed.
const (
	agentRunUser = "node"
	agentRunHome = "/home/node"
	// Under HOME because an unprivileged process cannot write /var/lib.
	agentStateDir = agentRunHome + "/.agentd"
)

// agentdPort is where the agent supervisor listens in the container.
const agentdPort = rt.AgentPort

// Resource backstops applied when BootSpec leaves the limits at 0. There is
// deliberately no --ulimit nproc: RLIMIT_NPROC is per-uid host-wide, and
// without userns-remap it would cap every root process on the host.
const (
	defaultPidsLimit   = 512
	defaultNofileLimit = 65536
)

// sandboxCaps is what every sandbox gets back after --cap-drop=ALL. Dropped
// from Docker's default: NET_RAW, MKNOD, SETFCAP, SETPCAP, SYS_CHROOT,
// AUDIT_WRITE. Each retained one is needed by a real path (setpriv drop, package managers, privileged ports).
var sandboxCaps = []string{
	"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID",
	"SETUID", "SETGID", "KILL", "NET_BIND_SERVICE",
}

// envdHostPath is the node's envd binary, bind-mounted into every non-service
// container. When absent the mount is skipped and the container still boots.
func envdHostPath() string {
	if p := os.Getenv("PD_ENVD_PATH"); p != "" {
		return p
	}
	return "/opt/pilldrops/bin/envd"
}

// cliHostPath is the node's static linux pilldrops CLI, bind-mounted into every
// sandbox so guest code can drive the platform with its own identity. Same
// file templatebuild bakes into Firecracker templates (PD_CLI_PATH).
func cliHostPath() string {
	if p := os.Getenv("PD_CLI_PATH"); p != "" {
		return p
	}
	return "/opt/pilldrops/bin/pilldrops-linux"
}

// agentdHostPath is the node's agentd binary, bind-mounted into agent sandboxes.
func agentdHostPath() string {
	if p := os.Getenv("PD_AGENTD_PATH"); p != "" {
		return p
	}
	return "/opt/pilldrops/bin/agentd"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// nonAgentBaseImages maps every non-agent template id and alias to its base
// image. A map rather than a switch so agentIDCollision can range over it.
// Ids that resolve to the debian default are omitted.
var nonAgentBaseImages = map[string]string{
	"code-interpreter": "python:3.11-slim-bookworm", "python-3.11": "python:3.11-slim-bookworm",
	"python-data-science": "python:3.11-slim-bookworm", "code-interpreter-v1": "python:3.11-slim-bookworm",
	"fastapi": "python:3.11-slim-bookworm", "openclaw": "python:3.11-slim-bookworm",
	"research": "python:3.11-slim-bookworm", "deep-research": "python:3.11-slim-bookworm",
	"web-scraper": "python:3.11-slim-bookworm", "scraper": "python:3.11-slim-bookworm",

	"node": "node:20-slim", "nextjs": "node:20-slim",
	"webhook-inbox": "node:20-slim", "webhook-tester": "node:20-slim",

	"go": "golang:1.22-bookworm", "go-1.22": "golang:1.22-bookworm",

	"rust": "rust:1.77-slim-bookworm", "rust-1.77": "rust:1.77-slim-bookworm",

	"playwright": "mcr.microsoft.com/playwright:v1.49.0-jammy", "playwright-test": "mcr.microsoft.com/playwright:v1.49.0-jammy",
	"e2e": "mcr.microsoft.com/playwright:v1.49.0-jammy",
}

// imageFor maps a template id or alias to its Docker image; unknown ids get
// the debian base.
func imageFor(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if m, ok := templates.AgentMetaFor(key); ok && m.BaseImage != "" {
		return m.BaseImage
	}
	if img, ok := nonAgentBaseImages[key]; ok {
		return img
	}
	return "debian:bookworm-slim"
}

// Runtime boots Docker-backed sandbox instances.
type Runtime struct {
	pulled   sync.Map // image -> struct{}, so an image is inspected/pulled once
	building sync.Map // enriched-image tag -> struct{}, so only one build runs at a time
	// imageAllowlist and imageRequireDigest mirror internal/httpapi's tenant
	// image policy (this package cannot import httpapi). They apply only to
	// tenant-supplied images, never to platform template pulls, or an
	// allowlist would block python-3.11/android on any cold node.
	imageAllowlist     []string
	imageRequireDigest bool
	// ociRuntime (SANDBOXD_DOCKER_RUNTIME, e.g. "runsc") runs toolbox sandboxes
	// under a non-default OCI runtime. Empty keeps runc.
	ociRuntime string
	// eng is the Engine API client for hot paths; nil falls back to the CLI.
	eng *engine
	// imageSource fetches an image from the artifact store. A function value so
	// this package does not link the store into every node binary.
	imageSource atomic.Pointer[func(context.Context, string) error]
}

// SetImageSource installs the artifact-store fetch for custom images, which
// exist in no registry.
func (r *Runtime) SetImageSource(fn func(ctx context.Context, tag string) error) {
	if fn == nil {
		r.imageSource.Store(nil)
		return
	}
	r.imageSource.Store(&fn)
}

// digestRefRe matches a digest-pinned image ref (name@sha256:<64 hex>).
var digestRefRe = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)

// New verifies the Docker daemon is reachable and reads the image-policy env
// vars once.
func New() (*Runtime, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, se, code, err := dockerOut(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil || code != 0 || strings.TrimSpace(out) == "" {
		return nil, dockerErr("docker daemon not reachable", err, code, se)
	}
	eng := newEngine()
	if eng != nil {
		slog.Info("Engine API available; exec takes the fast path (~18ms vs ~65ms per call)",
			"component", "docker")
	} else {
		slog.Warn("Engine API socket not reachable; falling back to the docker CLI for every operation",
			"component", "docker")
	}
	r := &Runtime{
		imageAllowlist:     splitCSV(os.Getenv("PILLDROPS_IMAGE_ALLOWLIST")),
		imageRequireDigest: envDigestRequireFlag(os.Getenv("PILLDROPS_IMAGE_REQUIRE_DIGEST")),
		ociRuntime:         strings.TrimSpace(os.Getenv("SANDBOXD_DOCKER_RUNTIME")),
		eng:                eng,
	}
	// Logged here as well as at the HTTP layer so a parse drift between the two shows.
	slog.Info("tenant image backstop resolved", "component", "docker",
		"allowlist_entries", len(r.imageAllowlist),
		"require_digest", r.imageRequireDigest)
	if r.ociRuntime != "" {
		slog.Info("docker sandboxes run under a non-default OCI runtime", "component", "docker", "runtime", r.ociRuntime)
	}
	return r, nil
}

// envDigestRequireFlag must parse exactly as the HTTP layer does, so it
// delegates to config.ParseBool rather than reimplementing it.
func envDigestRequireFlag(v string) bool {
	on, _ := config.ParseBool(v)
	return on
}

// splitCSV parses a comma-separated env value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// imageAllowed applies the registry allowlist and digest-pin policy. Callers
// must only pass a tenant-supplied image, never a platform template image.
func (r *Runtime) imageAllowed(img string) error {
	if r.imageRequireDigest && !digestRefRe.MatchString(img) {
		return fmt.Errorf("image must be pinned to a digest (name@sha256:...)")
	}
	if len(r.imageAllowlist) == 0 {
		return nil
	}
	for _, prefix := range r.imageAllowlist {
		if prefixMatchesRegistry(img, prefix) {
			return nil
		}
	}
	return fmt.Errorf("image registry not in allowlist")
}

// prefixMatchesRegistry reports whether img is covered by an allowlist entry.
// A bare-host entry must end at a component boundary so "ghcr.io" does not
// match "ghcr.io.evil.com".
func prefixMatchesRegistry(img, prefix string) bool {
	if !strings.HasPrefix(img, prefix) {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return true
	}
	rest := img[len(prefix):]
	return rest == "" || rest[0] == '/' || rest[0] == ':' || rest[0] == '@'
}

func (r *Runtime) Name() string { return "docker" }

// Capabilities reports Isolation: true, meaning a tenant boundary on a shared
// kernel, not a VM.
func (r *Runtime) Capabilities() rt.Caps { return DeclaredCaps() }

// DeclaredCaps is the capability declaration, readable without a live daemon.
// MultiPort is false because a container publishes one app port to the host.
// L4Forward is still true: a raw forward works, but only for the ports declared
// at create, which is the same constraint MultiPort=false describes.
func DeclaredCaps() rt.Caps {
	// NamedSnapshot is true, but disk-only: a docker capture is a committed
	// filesystem with no memory image, so a sandbox stamped out of one boots
	// cold and replays its start command. Every response says which.
	// Desktop: a container needs no extra capability to run Xvfb — only the
	// software in its image, which has_desktop answers per sandbox.
	return rt.Caps{Isolation: true, Snapshot: true, Checkpoint: true, Standby: true,
		NamedSnapshot: true, L4Forward: true, Desktop: true}
}

func (r *Runtime) Prepare(ctx context.Context, t rt.Template) error {
	if bs, ok := builtinServices[canonicalTemplate(t.Name)]; ok {
		return r.ensureImage(ctx, bs.image)
	}
	return r.ensureImage(ctx, imageFor(t.Name))
}

// ensureImage pulls img once if it is not present. It must not run the
// allowlist check: it is reached for platform template pulls too.
func (r *Runtime) ensureImage(ctx context.Context, img string) error {
	if _, ok := r.pulled.Load(img); ok {
		return nil
	}
	// runErr with code 0 means docker never ran, not that the image is present.
	inspectOut, _, code, runErr := dockerOut(ctx, "image", "inspect", img)
	_ = inspectOut
	if code != 0 || runErr != nil {
		// Artifact store first: a custom image exists in no registry.
		if err := r.fetchImage(ctx, img); err == nil {
			r.pulled.Store(img, struct{}{})
			return nil
		} else if !errors.Is(err, errNoImageSource) && !errors.Is(err, rt.ErrImageNotPublished) {
			// The store knew the image and failed to deliver it; a registry
			// error would point the operator at the wrong place.
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if _, se, code, err := dockerOut(pctx, "pull", img); err != nil || code != 0 {
			return dockerErr("pull "+img, err, code, se)
		}
	}
	r.pulled.Store(img, struct{}{})
	return nil
}

// errNoImageSource means no artifact store is configured; callers fall through
// to the registry.
var errNoImageSource = errors.New("docker: no artifact image source is configured")

// fetchImage materialises img from the shared artifact store.
func (r *Runtime) fetchImage(ctx context.Context, img string) error {
	fn := r.imageSource.Load()
	if fn == nil {
		return errNoImageSource
	}
	return (*fn)(ctx, img)
}

// snapImageTag is the local image a paused sandbox's filesystem is committed to.
func snapImageTag(sandboxID string) string { return "sandboxd-snap-" + dockerSafe(sandboxID) }

// dockerSafe reduces an id or name to what a docker image tag accepts.
func dockerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			return r
		default:
			return '-'
		}
	}, strings.ToLower(s))
}

func (r *Runtime) Boot(ctx context.Context, spec rt.BootSpec) (rt.Instance, error) {
	// This runtime has no per-sandbox egress enforcement: containers share the
	// bridge and the host chains see only the post-SNAT source. Refused, not
	// ignored — a sandbox that reads as confined and reaches everything is worse
	// than a create that fails.
	if spec.Egress != nil {
		return nil, errors.New("the docker runtime cannot enforce a per-sandbox egress policy; " +
			"run this sandbox on a firecracker node with FC_NETNS=1, or create it without one")
	}
	// Checked before resolveBuiltin fills in a platform image, so only the
	// tenant-supplied image is policy-checked.
	if spec.Template.Image != "" {
		if err := r.imageAllowed(spec.Template.Image); err != nil {
			return nil, err
		}
	}
	spec, devices := resolveBuiltin(spec)
	img := spec.Template.Image // image-backed service template (or builtin service)
	if img == "" {
		img = r.templateImage(ctx, spec.Template.Name)
	} else if warm := r.builtinWarmImage(ctx, spec.Template.Name); warm != "" {
		img = warm
	}
	return r.bootImage(ctx, spec, img, devices)
}

// builtinWarmImage returns a builtin service's pre-warmed local image when this
// node has baked one, else "".
func (r *Runtime) builtinWarmImage(ctx context.Context, name string) string {
	bs, ok := builtinServices[canonicalTemplate(name)]
	if !ok || bs.warmTag == "" {
		return ""
	}
	key := "warm:" + bs.warmTag
	if _, ok := r.pulled.Load(key); ok {
		return bs.warmTag
	}
	// As in ensureImage, code 0 with a run error means docker never ran.
	if _, _, code, runErr := dockerOut(ctx, "image", "inspect", bs.warmTag); code == 0 && runErr == nil {
		r.pulled.Store(key, struct{}{})
		return bs.warmTag
	}
	return ""
}

// templateImage returns the enriched image when it is already built, else the
// base image, kicking off the build in the background so creates never block on it.
func (r *Runtime) templateImage(ctx context.Context, name string) string {
	base := imageFor(name)
	df, ok := enrichedDockerfiles[canonicalTemplate(name)]
	if !ok {
		// Every platform image carries the screen stack, so a template with no
		// tooling to add still gets one built for it. This is what makes
		// PD_DESKTOP=1 a property of the sandbox rather than of the template.
		df = "FROM " + base
	}
	df += DesktopLayer
	// The tag hashes the Dockerfile so an edited recipe rebuilds on every node.
	h := fnv.New32a()
	_, _ = h.Write([]byte(df))
	tag := fmt.Sprintf("pilldrops/%s:%x", canonicalTemplate(name), h.Sum32())
	if _, built := r.pulled.Load("built:" + tag); built {
		return tag
	}
	if _, _, code, _ := dockerOut(ctx, "image", "inspect", tag); code == 0 {
		r.pulled.Store("built:"+tag, struct{}{})
		return tag
	}
	r.buildEnrichedAsync(tag, df) // non-blocking; ready for a later create
	return base
}

// buildEnrichedAsync builds an enriched image in the background, at most once
// at a time per tag. A failed build is left unmarked so a later create retries.
func (r *Runtime) buildEnrichedAsync(tag, dockerfile string) {
	if _, loaded := r.building.LoadOrStore(tag, struct{}{}); loaded {
		return // a build for this tag is already in flight
	}
	go func() {
		defer r.building.Delete(tag)
		bctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(bctx, "docker", "build", "-t", tag, "-")
		cmd.Stdin = strings.NewReader(dockerfile)
		if err := cmd.Run(); err == nil {
			r.pulled.Store("built:"+tag, struct{}{})
		}
	}()
}

// ensureSandboxNet creates the shared bridge network with icc=false so
// containers cannot reach each other. Idempotent.
func (r *Runtime) ensureSandboxNet(ctx context.Context) error {
	_, _, code, _ := dockerOut(ctx, "network", "inspect", "sandboxd-net")
	if code == 0 {
		return nil
	}
	_, se, code, err := dockerOut(ctx, "network", "create",
		"-o", "com.docker.network.bridge.enable_icc=false", "sandboxd-net")
	if err != nil || code != 0 {
		if strings.Contains(strings.ToLower(se), "already exists") {
			return nil
		}
		return fmt.Errorf("create sandboxd-net: %s", se)
	}
	return nil
}

// CheckIsolation verifies sandboxd-net exists with icc=false. Satisfies
// nodeboot.IsolationChecker structurally, since nodeboot imports this package.
func (r *Runtime) CheckIsolation(ctx context.Context) error {
	if err := r.ensureSandboxNet(ctx); err != nil {
		return fmt.Errorf("ensure sandboxd-net: %w", err)
	}
	out, se, code, err := dockerOut(ctx, "network", "inspect", "sandboxd-net",
		"--format", `{{index .Options "com.docker.network.bridge.enable_icc"}}`)
	if err != nil || code != 0 {
		return fmt.Errorf("inspect sandboxd-net: %s", se)
	}
	if got := strings.TrimSpace(out); got != "false" {
		return fmt.Errorf("sandboxd-net icc=%q, want %q", got, "false")
	}
	return nil
}

// portBindRetries bounds `docker run` retries after a host-port bind race.
const portBindRetries = 3

// dedupePublishPorts returns appPort followed by each distinct extra port.
func dedupePublishPorts(appPort int, extra []int) []int {
	out := []int{appPort}
	seen := map[int]bool{appPort: true}
	for _, p := range extra {
		if p <= 0 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// isPortBindConflict reports whether a `docker run` failure is the transient
// host-port bind race.
func isPortBindConflict(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "bind host port") ||
		strings.Contains(s, "port is already allocated") ||
		(strings.Contains(s, "address already in use") && strings.Contains(s, "port"))
}

// mappedPort resolves the host address Docker published for a container port.
func mappedPort(ctx context.Context, cid string, port int) (string, error) {
	out, se, code, err := dockerOut(ctx, "port", cid, strconv.Itoa(port)+"/tcp")
	if err != nil || code != 0 {
		return "", dockerErr("docker port "+cid, err, code, se)
	}
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l, nil // "127.0.0.1:49153"
		}
	}
	return "", fmt.Errorf("no published port for %s", cid)
}

// dockerErr renders a docker CLI failure, wrapping err only when it is non-nil
// so a plain non-zero exit does not print "%!w(<nil>)".
func dockerErr(op string, err error, code int, stderr string) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case err != nil && msg != "":
		return fmt.Errorf("%s: %w: %s", op, err, msg)
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	case msg != "":
		return fmt.Errorf("%s: exit %d: %s", op, code, msg)
	default:
		return fmt.Errorf("%s: exit %d", op, code)
	}
}

// dockerOut runs a docker CLI command. A non-zero exit is reported via code
// with err nil; err is set only when the command could not run.
func dockerOut(ctx context.Context, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return so.String(), se.String(), ee.ExitCode(), nil
	}
	return so.String(), se.String(), 0, err
}

var _ rt.Runtime = (*Runtime)(nil)

// storageCapUnenforced counts sandboxes booted without an enforced disk cap.
var storageCapUnenforced atomic.Int64

func noteStorageCapUnenforced(declaredMB int) {
	n := storageCapUnenforced.Add(1)
	// Once per process: every sandbox on this host has the same property.
	if n == 1 {
		slog.Warn("this host's storage driver does not enforce --storage-opt size (it either "+
			"rejects the option or accepts and ignores it), so sandbox writable disks are NOT "+
			"capped. Sandboxes are still metered at their declared size, so a runaway workload "+
			"can fill the host disk and be under-billed. Firecracker sizes its overlay per "+
			"sandbox and is unaffected.", "component", "docker", "declared_mb", declaredMB)
	}
}

// StorageCapUnenforced reports how many sandboxes booted without a disk cap on this host.
func StorageCapUnenforced() int64 { return storageCapUnenforced.Load() }

// productionNode reports whether SANDBOXD_PRODUCTION is set, parsed the same
// way as every other boolean env var.
func productionNode() bool { return config.Bool("SANDBOXD_PRODUCTION", false) }
