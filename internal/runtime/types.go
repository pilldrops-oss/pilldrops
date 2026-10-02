// Package runtime defines the runtime contract the daemon drives. Implementations
// are firecracker, docker and process (dev only); internal/nodeboot selects one.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// Ports the platform owns inside every sandbox, declared once so runtimes cannot drift.
const (
	// TerminalPort is envd's web terminal.
	TerminalPort = 7681
	// DesktopPort is envd's noVNC viewer, bound only when PD_DESKTOP=1 asked
	// for a screen. Reserved on every sandbox so the proxy can reach it without
	// the template having declared it.
	DesktopPort = 6080
)

// IsPlatformPort reports whether a port belongs to the platform rather than to a user app.
func IsPlatformPort(port int) bool {
	return port == TerminalPort || port == DesktopPort
}

// Sentinel errors the daemon and API translate to status codes.
var (
	ErrNotFound = errors.New("sandbox not found")
	// ErrNodeBusy: this node cannot take the sandbox right now (full, no boot
	// slot, disk pressure) though another node might. The control plane
	// re-places the create instead of failing it.
	ErrNodeBusy    = errors.New("node busy")
	ErrPaused      = errors.New("sandbox is paused")
	ErrNoAppOnPort = errors.New("no app listening on that port")
	// ErrPathNotFound means the sandbox is fine and the path inside it is not
	// there. Distinct from ErrNotFound, which names the sandbox: reporting a
	// mistyped path as a missing sandbox sends the caller to check the one
	// thing that was right.
	ErrPathNotFound = errors.New("no such path in the sandbox")
	// ErrPathRefused means the path resolves outside the sandbox's workspace.
	// The caller's input, and refused identically every time — so an HTTP
	// caller must hear a 4xx, not a 503 telling them to try again.
	//
	// Runtimes word the refusal differently and deliberately vaguely (an escape
	// attempt and a typo must not be distinguishable from outside); this is how
	// they agree on what it means without agreeing on what it says.
	ErrPathRefused = errors.New("path refused by the sandbox")
	// ErrForeignOrg means the requesting org does not own the sandbox. HTTP callers
	// must render it as 404, same as ErrNotFound, or the preview surface becomes an
	// existence oracle.
	ErrForeignOrg = errors.New("sandbox not found")
	// ErrUnsupported means the runtime does not implement an optional capability.
	ErrUnsupported = errors.New("not supported by this runtime")
	// ErrTemplateUnknown means the runtime cannot boot the requested template.
	ErrTemplateUnknown = errors.New("unknown template")
	// ErrRootfsMismatch means a capture was taken against a different build of
	// its template's read-only root image than this node holds under that name.
	// Permanent for this node, so callers must render it as a refusal rather
	// than a retryable one. See internal/vm/rootfsdigest.go for why the template
	// name is not an identity.
	ErrRootfsMismatch = errors.New("snapshot was captured against a different root image than this node holds")
	// ErrCPUTooPoor means a memory snapshot was captured on a CPU providing
	// instructions this node does not. Permanent for this node like
	// ErrRootfsMismatch, and for the same reason: the alternative is a 201 and
	// a guest that dies with SIGILL. See internal/vm/cpucheck.go.
	ErrCPUTooPoor = errors.New("snapshot was captured on a CPU with instructions this node does not provide")
	// ErrImageNotPublished means the artifact store has no build for the tag.
	// Not a failure: plain registry images answer this and fall through to a pull.
	ErrImageNotPublished = errors.New("image is not published to the artifact store")
	// ErrProtectedProcess means the pid is the sandbox's own keepalive, whose
	// death ends the sandbox: the caller's input, so a 4xx.
	ErrProtectedProcess = errors.New("that process keeps the sandbox alive and cannot be signalled")
	// ErrWontStayUp means a revive's start succeeded and a state read then
	// showed the instance stopped again: a crash, not an engine still settling.
	ErrWontStayUp = errors.New("would not stay up after restart")
)

// Resources is a per-sandbox CPU/RAM allocation.
type Resources struct {
	VCPUs    int
	MemoryMB int
	// StorageMB is the template's default writable disk; the golden snapshot
	// is built at this size so a default-size create can restore from it.
	StorageMB int
}

// AppLogPath is where StartServer captures the app's stdout+stderr, relative to
// the sandbox root so it travels with snapshots.
const AppLogPath = ".sandboxd/app.log"

// Caps advertises what a runtime can do.
type Caps struct {
	// Isolation reports whether the runtime enforces any tenant boundary, not
	// what kind: docker sets it true while sharing the host kernel. Use
	// LevelFor for the distinction.
	Isolation bool
	Snapshot  bool // true = pause frees RAM via memory snapshot; false = SIGSTOP only
	// MemoryRestore is true when Restore brings the guest's processes back with
	// it, so a resume must NOT relaunch the start command — it would start a
	// second copy of the app beside the one the memory image already holds.
	// This is CheckpointInfo.Kind == "memory" for the implicit pause snapshot,
	// which Restore has no way to report. Distinct from Snapshot: docker's
	// pause frees RAM (Snapshot) by committing a filesystem and dropping the
	// container, and its resume boots that image cold (MemoryRestore false).
	MemoryRestore bool
	// MultiPort is true when any guest port is reachable on the preview URL;
	// false means one published app port per sandbox.
	MultiPort bool
	// WarmRestoreStorageMB is the only writable-disk size the warm (golden
	// snapshot) boot path can serve, 0 if there is none. Any other size cold
	// boots, since the overlay cannot be resized under a restored kernel.
	WarmRestoreStorageMB int
	// Checkpoint is true when the runtime implements Checkpointer: named save
	// points a sandbox can be rolled back to in place.
	Checkpoint bool
	// NamedSnapshot is true when the runtime implements SnapshotBooter: a
	// capture can be published under an org-owned name and *new* sandboxes
	// stamped out of it. Checkpoint is the same capture rolled back into the
	// same sandbox; this one produces a different machine, which is why
	// firecracker needs per-sandbox namespaces for it and not for Checkpoint.
	NamedSnapshot bool
	// Standby is true when the runtime's instances implement Freezer: an idle
	// sandbox can be frozen in RAM and thawed in milliseconds, keeping its
	// processes and open sockets. Independent of Snapshot, which is the
	// disk tier.
	Standby bool
	// EgressPolicy is true when this runtime enforces BootSpec.Egress. It
	// depends on node configuration, not just the engine: firecracker reports
	// it only with per-sandbox namespaces (FC_NETNS), since the shared bridge
	// has nowhere to hang a per-sandbox rule.
	EgressPolicy bool
	// EgressDomains is true when this runtime also enforces the DOMAIN entries
	// in BootSpec.Egress, which needs the name layer that reads TLS SNI and the
	// HTTP Host header. Narrower than EgressPolicy: an address and CIDR rule
	// set is decided entirely in the kernel chain and needs nothing else.
	// False means a rule set naming a domain is REFUSED, never accepted and
	// left unenforced — that is the whole point of a separate bit.
	EgressDomains bool
	// TagNetworks is true when this runtime can put two sandboxes that named
	// the same network tag in reach of each other. Like EgressPolicy it depends
	// on node configuration, not just the engine: firecracker reports it only
	// with per-sandbox namespaces (FC_NETNS), since the partition is a rule in
	// the host's netns table. False is the shipped default everywhere and means
	// a create naming tags is refused, never accepted and ignored.
	TagNetworks bool
	// L4Forward is true when a raw TCP/UDP stream can be spliced to a guest
	// port, which needs ListenTarget to resolve a port the runtime actually
	// serves. Docker reports it true but honours only the ports declared at
	// create (see MultiPort); a runtime with one fixed loopback port is false.
	L4Forward bool
	// NestedDocker is true when a sandbox can run a container daemon of its
	// own. It follows from owning a kernel, so it is not configurable: a
	// shared-kernel runtime would need SYS_ADMIN and NET_ADMIN inside the
	// sandbox, which is the boundary it exists to keep. See docs/runtimes.md
	// §Boundaries. The `docker` template is refused where this is false.
	NestedDocker bool
	// Desktop is true when this runtime can host an X server, which is a
	// property of the engine and not of any one sandbox. It is true on both
	// shipped runtimes and gates nothing by itself: whether a given sandbox
	// can serve a screen also needs the image to hold the software, which is
	// what the per-sandbox has_desktop combines. A runtime that cannot host X
	// reports false and clients grey the action from GET /v1/capabilities.
	Desktop bool
}

// Template describes a sandbox image.
type Template struct {
	Name string
	// Image, when set, makes this a service template (e.g. "postgres:16") run
	// with its own entrypoint instead of the base sandbox image.
	Image string
	Ports []int    // extra ports to publish (e.g. 5432)
	Env   []string // "NAME=VALUE" env for the container
}

// BootSpec is everything an engine needs to start one instance.
type BootSpec struct {
	SandboxID string
	Template  Template
	// VCPU and MemMiB are also what the meter prices, so the process runtime
	// reports them even though it does not schedule on them.
	VCPU   int
	MemMiB int
	// StorageMB caps the writable disk. Docker's --storage-opt size is only
	// enforced by some drivers; process ignores it.
	StorageMB int
	// PidsLimit caps the sandbox pid cgroup (fork-bomb guard). 0 = runtime default.
	PidsLimit int
	// NofileLimit is the container's nofile ulimit. 0 = runtime default.
	// There is deliberately no nproc limit: RLIMIT_NPROC is per-uid host-wide,
	// so without userns-remap it would cap every root process on the host.
	NofileLimit int
	// Egress is the sandbox's network policy; nil is unrestricted. An engine
	// that cannot enforce one must refuse the boot rather than ignore it: a
	// sandbox that reads as confined and is not is worse than a failed create.
	Egress *egress.Rules
	// Identity is the sandbox's own platform credential, delivered into the
	// guest after boot (vsock credential channel on Firecracker, docker cp on
	// Docker). Zero on a resume: the guest already holds it. A delivery that
	// fails fails the boot; a sandbox is never handed out without its identity.
	Identity Identity
}

// Identity is what a guest needs to call the platform as itself. The token
// exists on the node only for the length of the push; the daemon keeps its
// hash on the sandbox row and destroy revokes it.
type Identity struct {
	Token     string // pd_sb_… bearer
	APIURL    string // PILLDROPS_API_URL the guest should call
	SandboxID string // PD_SANDBOX_ID
	Domain    string // PD_SANDBOX_DOMAIN; "" when the node has none
}

// SnapshotRef locates a persisted pause snapshot. Opaque to the daemon.
type SnapshotRef struct {
	Path string // engine-defined location (tar for process; state+mem for firecracker)
	// Name is the checkpoint's name when the ref is a named checkpoint rather
	// than the implicit pause snapshot. Engines that key on a string (docker's
	// image tag) read it instead of parsing Path.
	Name string
}

func (s SnapshotRef) IsZero() bool { return s.Path == "" }

// ExecSpec is one command to run inside a sandbox.
type ExecSpec struct {
	Cmd     string
	Cwd     string // relative to sandbox root; "" = root
	Env     map[string]string
	Timeout time.Duration // 0 = engine default
	// Detach starts the command in the background and returns its Pid at once.
	// Output goes to a log inside the sandbox, not to the result.
	Detach bool
	// Stdin is delivered whole to the command's standard input; nil is /dev/null.
	Stdin []byte
	// RawOutput lifts the guest's output cap for a byte-exact stream (tar egress).
	RawOutput bool
}

// ExecResult is the outcome of an ExecSpec.
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
	// Pid is the process id of a detached command (ExecSpec.Detach); 0 otherwise.
	Pid int
}

// StreamFrame is one incremental exec output message. Exited marks the final
// frame, after which ExitCode is valid.
type StreamFrame struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Exited    bool
	Truncated bool
}

// StreamExecutor is an optional Instance capability that streams exec output.
// onFrame is called synchronously from the exec loop and must return promptly.
type StreamExecutor interface {
	ExecStream(ctx context.Context, s ExecSpec, onFrame func(StreamFrame)) error
}

// DirEntry is one directory listing entry as the SDK filesystem API returns it.
type DirEntry struct {
	Name     string    `json:"name"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// FileInfo is a file stat result as the SDK filesystem API returns it.
type FileInfo struct {
	Name     string    `json:"name"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Mode     string    `json:"mode"`
	Modified time.Time `json:"modified"`
}

// Proc is one process visible inside the sandbox.
type Proc struct {
	PID int    `json:"pid"`
	Cmd string `json:"cmd"`
}

// InstanceMetrics is a point-in-time compute reading for metering/dashboards.
type InstanceMetrics struct {
	VCPU       int
	MemMiB     int
	ExecCount  int64
	UptimeSecs float64
}

// LiveStats is instantaneous per-instance resource usage, unlike the cumulative
// counters in InstanceMetrics.
type LiveStats struct {
	CPUPct     float64 // whole-instance CPU utilisation, percent (100 = one core)
	MemUsedMiB int     // resident memory in MiB
	DiskUsedMB int     // writable-layer / workspace bytes in MB (-1 if unknown)
}

// LiveStatser is an optional Instance capability; the daemon omits live fields
// when it is absent.
type LiveStatser interface {
	LiveStats(ctx context.Context) (LiveStats, error)
}

// FileSystem is the file API exposed by every Instance. Every call crosses a
// process boundary and can block, hence the context.
type FileSystem interface {
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, data []byte) error
	List(ctx context.Context, path string) ([]DirEntry, error)
	Mkdir(ctx context.Context, path string) error
	Move(ctx context.Context, src, dst string) error
	Remove(ctx context.Context, path string) error
	Stat(ctx context.Context, path string) (FileInfo, error)
}

// Liveness is an optional Instance capability: Alive reports whether the
// VMM/container process still runs, so the daemon can reap a crashed sandbox.
type Liveness interface {
	Alive() bool
}

// OOMKilledReporter is an optional Liveness companion: whether an instance
// that Alive reports dead was killed for exceeding its memory limit.
type OOMKilledReporter interface {
	OOMKilled() bool
}

// Reviver is an optional Liveness companion. Casualty reports that the last
// Alive read found the instance stopped by its own engine restarting (dockerd
// without live-restore) rather than dead; Revive starts it again and returns
// the instance to use from then on, its published ports re-read.
type Reviver interface {
	Casualty() bool
	Revive(ctx context.Context) (Instance, error)
}

// Instance is one running sandbox's compute. It is nil while paused/stopped.
type Instance interface {
	ID() string
	Exec(ctx context.Context, s ExecSpec) (ExecResult, error)
	FS() FileSystem
	Procs() ([]Proc, error)
	Signal(pid, sig int) error
	// ListenTarget returns the host:port the preview proxy dials for an app
	// port, or "" if nothing is listening yet.
	ListenTarget(port int) (string, error)
	// TerminalTarget returns the host:port of envd's web terminal, which the
	// preview proxy dials when the sandbox has no user app.
	TerminalTarget() (string, error)
	// StartServer launches a long-lived server inside the sandbox with env
	// (may be nil) injected alongside $PORT.
	StartServer(ctx context.Context, cmd string, env map[string]string) error
	Snapshot(ctx context.Context, dst SnapshotRef) error
	Stop(ctx context.Context) error
	Metrics() InstanceMetrics
}

// OOMReporter is an optional Instance capability: cumulative OOM kills since
// boot. The source must be sandbox-private (guest dmesg, or the container's own
// cgroup memory.events).
type OOMReporter interface {
	OOMKillCount(ctx context.Context) (int, error)
}

// TerminalAuthorizer is an optional Instance capability: the token envd's web
// terminal requires on /ws. The proxy attaches it server-side; the browser
// never sees it.
type TerminalAuthorizer interface {
	TerminalToken() (string, error)
}

// DiskUsager is an optional Instance capability: storage the sandbox occupies,
// in MB. Firecracker does not implement it; with no discard, the overlay's
// allocated blocks are a high-water mark, not live usage.
type DiskUsager interface {
	DiskUsageMB(ctx context.Context) (int, error)
}

// DiskPressured is an optional Runtime capability: whether the engine is
// already refusing work because its filesystem is too full. The create path
// asks before it materialises a template, because that fetch — a named
// snapshot's memory image most of all — is larger than anything the boot then
// writes and happens above every guard inside the engine. A runtime that does
// not track it simply does not implement this.
type DiskPressured interface {
	DiskPressured() bool
}

// StorageCapReporter is an optional Runtime capability: how many sandboxes
// booted without an enforced writable-disk cap.
type StorageCapReporter interface {
	StorageCapUnenforced() int64
}

// Runtime is the platform engine. Selected once at startup by capability probe.
type Runtime interface {
	Name() string
	Capabilities() Caps
	Prepare(ctx context.Context, t Template) error
	Boot(ctx context.Context, spec BootSpec) (Instance, error)
	Restore(ctx context.Context, snap SnapshotRef, spec BootSpec) (Instance, error)
	RecoverOrphans() []Instance
}

// Adopter is implemented by runtimes whose instances outlive the daemon; Adopt
// reattaches to the instance for sandboxID. ErrNotFound means there is none;
// any other error means the runtime could not tell, and nothing may be decided
// from it.
type Adopter interface {
	Adopt(ctx context.Context, sandboxID string) (Instance, error)
}

// Sweeper garbage-collects instances not in known, the full set of sandbox ids
// the daemon tracks after restore.
type Sweeper interface {
	SweepUnknown(known []string)
}

// StreamFS is an optional FileSystem capability: the runtime moves file bytes
// without buffering the whole file on the node. A FileSystem without it is
// served through Read/Write.
type StreamFS interface {
	WriteFrom(ctx context.Context, path string, r io.Reader) (int64, error)
	ReadTo(ctx context.Context, path string, w io.Writer) error
}

// BootTyper is an optional Instance capability: how the runtime brought this
// instance up ("snapshot" for a restore from a golden, "cold" for a boot).
// Without it the daemon can only guess from the request.
type BootTyper interface {
	BootType() string
}

// EnvdProtocoler is an optional Instance capability: the envdproto level of the
// guest agent, so a feature newer than level 1 is refused as unsupported rather
// than failing inside the guest.
type EnvdProtocoler interface {
	EnvdProtocol() int
}

// EnvSetter is an optional Instance capability: the runtime can update the
// guest's session environment after boot, so a config change reaches shells
// and not only the next exec.
type EnvSetter interface {
	SetEnv(ctx context.Context, env map[string]string) error
}

// Freezer is an optional Instance capability: stop the sandbox's compute while
// keeping its memory, processes and open sockets resident, and start it again.
// It sits on the Instance rather than the Runtime because, unlike Forker and
// Checkpointer, it neither creates nor replaces one. Both operations are
// idempotent from the caller's side: the daemon's state machine, not the
// engine, decides when they are legal.
type Freezer interface {
	Freeze(ctx context.Context) error
	Thaw(ctx context.Context) error
}

// SnapshotDeleter removes what a pause snapshot keeps beyond the Instance:
// Docker's committed image, Firecracker's kept overlay, tap and records. snap is
// the ref the daemon passed to Snapshot, zero when the sandbox was never paused.
// It must not error when there is nothing to delete.
type SnapshotDeleter interface {
	DeleteSnapshot(ctx context.Context, sandboxID string, snap SnapshotRef) error
}

// ForkSource is what a fork copies: a running parent's live instance, or a
// paused parent's snapshot when Parent is nil.
type ForkSource struct {
	Parent   Instance
	Snapshot SnapshotRef
}

// ForkInfo is what the copy cost the parent.
type ForkInfo struct {
	// ParentPaused is how long a running parent stood still; zero for a snapshot fork.
	ParentPaused time.Duration
}

// Networks is a sandbox's membership in the opt-in tag networks, as the
// runtime needs it. Sets are opaque keys the daemon has already scoped to the
// org, so a runtime never has to know what an org is; Slug is the name peers
// resolve the sandbox by. A zero Networks means "no membership": the sandbox
// reaches no peer and none reaches it, which is the default.
type Networks struct {
	Sets []string
	Slug string
}

// Networker is an optional Instance capability: the sandbox can be put into
// opt-in tag networks. SetNetworks replaces the membership — it is not
// additive — and must take effect on the running sandbox without a restart. It
// returns the address peers reach the sandbox on, "" when the membership is
// empty. A zero Networks is how a sandbox leaves every network; the runtime
// must also drop a destroyed sandbox's membership itself, because a stale
// entry would make whatever next takes its address reachable by its
// predecessor's peers.
type Networker interface {
	SetNetworks(ctx context.Context, n Networks) (privateIP string, err error)
}

// Forker is a Runtime that can copy a sandbox, memory, disk and running
// processes, into a new instance. The child comes up running either way.
type Forker interface {
	Fork(ctx context.Context, src ForkSource, spec BootSpec) (Instance, ForkInfo, error)
}

// CheckpointInfo is what a capture cost and what it holds.
type CheckpointInfo struct {
	// Paused is how long the guest stood still; zero when the engine captures
	// without stopping it (docker commit).
	Paused time.Duration
	// Kind is "memory" when the capture includes RAM and running processes, and
	// "disk" when it is the filesystem only. It decides whether a restore has to
	// relaunch the sandbox's start command.
	Kind      string
	SizeBytes int64
}

// Checkpointer is a Runtime that can take named save points of a sandbox and
// roll it back to one in place: same id, URL, token and address. dst.Path is a
// directory the daemon owns and has already created; the engine writes whatever
// a restore of it needs into that directory.
type Checkpointer interface {
	Checkpoint(ctx context.Context, inst Instance, dst SnapshotRef) (CheckpointInfo, error)
	RestoreCheckpoint(ctx context.Context, inst Instance, src SnapshotRef, spec BootSpec) (Instance, error)
}

// PausedCheckpointer is an optional Checkpointer capability: capture a paused
// sandbox from its pause snapshot, without starting it. A runtime without it
// has its paused sandboxes woken for the capture and paused again.
type PausedCheckpointer interface {
	CheckpointPaused(ctx context.Context, sandboxID string, snap, dst SnapshotRef) (CheckpointInfo, error)
}

// PortableCapture is an optional Runtime capability: make a capture directory
// self-contained, so every byte a restore needs is inside it and it can be
// moved to another machine, and undo that on arrival.
//
// A runtime whose Checkpoint already writes the whole machine into the
// directory does not implement this — firecracker's snapfile, memfile and
// overlay are the capture. Docker's is a committed image plus a file naming
// it, and the image is not in the directory, so a docker capture that leaves
// the node without Externalize restores to "checkpoint image is gone".
type PortableCapture interface {
	Externalize(ctx context.Context, dir string) error
	Internalize(ctx context.Context, dir string) error
}

// SnapshotBooter is a Runtime that can boot a *new* sandbox from a capture — the
// other half of Checkpointer, which rolls the same sandbox back. src.Path is a
// directory the node has already materialised, holding whatever that engine's
// Checkpoint wrote; the new sandbox gets its own id, address, token and
// hostname, and its start command is not run, because the capture is already
// past it.
type SnapshotBooter interface {
	// The returned kind is CheckpointInfo.Kind for what was booted: "memory"
	// when the machine came back with its processes running, "disk" when only
	// the filesystem did. The engine is the only thing that knows, so it says
	// so rather than leaving the daemon to infer it from the runtime's name.
	BootFromSnapshot(ctx context.Context, src SnapshotRef, spec BootSpec) (Instance, string, error)
}
