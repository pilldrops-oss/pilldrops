// Package runtimetest provides an in-memory Runtime double for daemon tests.
package runtimetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Fake is an in-memory Runtime. Boot returns a *fakeInstance that keeps files
// in a map and records every Exec so tests can assert on call history.
type Fake struct {
	mu        sync.Mutex
	instances map[string]*fakeInstance
	// BootErr, if set, is returned by the next Boot call.
	BootErr error
	// MultiPort makes Capabilities advertise multi-port preview.
	MultiPort bool
	// TagNetworks makes Capabilities advertise tag networks and SetNetworks
	// record what it was asked for. False (the default) is a runtime that
	// refuses a create naming networks.
	TagNetworks bool
	// EgressPolicy makes Capabilities advertise egress enforcement. False (the
	// default) is a runtime that refuses a create carrying network rules.
	EgressPolicy bool
	// EgressDomains makes Capabilities advertise the name layer, so a rule set
	// naming a domain is accepted. False (the default) refuses one.
	EgressDomains bool
	// networks is the last membership pushed per sandbox, for assertions.
	networks map[string][]string
	// CheckpointKind is the Kind Checkpoint reports; "" means "memory".
	CheckpointKind string
	// MemoryRestore makes Capabilities report that a resume brings the guest's
	// processes back with it, as firecracker's memory snapshot does. False (the
	// default) is a runtime whose resume boots cold and replays the start command.
	MemoryRestore bool
	// CheckpointErr, if set, fails the next Checkpoint call.
	CheckpointErr error

	// restored records every RestoreCheckpoint as "<sandbox>/<name>".
	restored []string
	// snapshotBooted records every BootFromSnapshot as "<sandbox>@<path>".
	snapshotBooted []string
	// SnapshotBootKind is the kind BootFromSnapshot reports; "" means "memory".
	SnapshotBootKind string
	// NoSnapshotBoot makes Capabilities report NamedSnapshot false. The method
	// stays — a runtime that advertises false and implements the interface is a
	// real shape, and the daemon must gate on the capability, not on the assertion.
	NoSnapshotBoot bool

	// bootSpecs records every Boot/Restore request, newest last.
	bootSpecs []rt.BootSpec
}

// LastBootSpec returns the most recent spec the daemon booted or restored with.
func (f *Fake) LastBootSpec() (rt.BootSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bootSpecs) == 0 {
		return rt.BootSpec{}, false
	}
	return f.bootSpecs[len(f.bootSpecs)-1], true
}

func NewFake() *Fake {
	return &Fake{instances: make(map[string]*fakeInstance)}
}

func (f *Fake) Name() string { return "fake" }
func (f *Fake) Capabilities() rt.Caps {
	return rt.Caps{
		Isolation: false, Snapshot: true, Checkpoint: true, Standby: true,
		MultiPort: f.MultiPort, TagNetworks: f.TagNetworks, L4Forward: true,
		MemoryRestore: f.MemoryRestore,
		EgressPolicy:  f.EgressPolicy,
		EgressDomains: f.EgressDomains,
		NamedSnapshot: !f.NoSnapshotBoot,
	}
}

// setNetworks records what an instance was asked to join. The private address
// is synthetic but stable, which is all a caller asserting on the API needs.
func (f *Fake) setNetworks(id string, n rt.Networks) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.networks == nil {
		f.networks = map[string][]string{}
	}
	if len(n.Sets) == 0 {
		delete(f.networks, id)
		return "", nil
	}
	f.networks[id] = append([]string(nil), n.Sets...)
	return "10.128.0.2", nil
}

// NetworksOf returns the set keys the daemon last pushed for a sandbox.
func (f *Fake) NetworksOf(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networks[id]
}
func (f *Fake) Prepare(context.Context, rt.Template) error { return nil }

func (f *Fake) Boot(_ context.Context, spec rt.BootSpec) (rt.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootSpecs = append(f.bootSpecs, spec)
	if f.BootErr != nil {
		return nil, f.BootErr
	}
	inst := &fakeInstance{
		rtF:   f,
		id:    spec.SandboxID,
		files: make(map[string][]byte),
		start: time.Now(),
	}
	f.instances[spec.SandboxID] = inst
	return inst, nil
}

func (f *Fake) Restore(_ context.Context, _ rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	return f.Boot(context.Background(), spec)
}

// Fork boots a fresh instance carrying the parent's files, so a test can tell
// a copy from a cold boot.
func (f *Fake) Fork(_ context.Context, src rt.ForkSource, spec rt.BootSpec) (rt.Instance, rt.ForkInfo, error) {
	inst, err := f.Boot(context.Background(), spec)
	if err != nil {
		return nil, rt.ForkInfo{}, err
	}
	if p, ok := src.Parent.(*fakeInstance); ok {
		p.mu.Lock()
		for k, v := range p.files {
			inst.(*fakeInstance).files[k] = v
		}
		p.mu.Unlock()
	}
	return inst, rt.ForkInfo{ParentPaused: time.Millisecond}, nil
}

// Checkpoint writes a marker file into the directory the daemon made, so a test
// can tell a real capture from a no-op, and reports CheckpointKind.
func (f *Fake) Checkpoint(_ context.Context, inst rt.Instance, dst rt.SnapshotRef) (rt.CheckpointInfo, error) {
	if f.CheckpointErr != nil {
		return rt.CheckpointInfo{}, f.CheckpointErr
	}
	kind := f.CheckpointKind
	if kind == "" {
		kind = "memory"
	}
	// The marker names the machine, not the capture's label: a capture's bytes
	// are a function of what was captured, so two captures of an unchanged
	// machine are identical and a content digest over them can dedupe.
	body := []byte(inst.ID())
	if err := os.WriteFile(filepath.Join(dst.Path, "payload"), body, 0o600); err != nil {
		return rt.CheckpointInfo{}, err
	}
	// The sandbox's files go into the capture too, so a restore can be checked
	// for carrying the data it was taken from and not merely for returning
	// bytes. A capture that loses the filesystem is the failure worth catching.
	i, ok := inst.(*fakeInstance)
	if !ok {
		return rt.CheckpointInfo{}, fmt.Errorf("checkpoint: not a fake instance")
	}
	i.mu.Lock()
	snap, err := json.Marshal(i.files)
	i.mu.Unlock()
	if err != nil {
		return rt.CheckpointInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(dst.Path, "files.json"), snap, 0o600); err != nil {
		return rt.CheckpointInfo{}, err
	}
	return rt.CheckpointInfo{Kind: kind, SizeBytes: int64(len(body) + len(snap)), Paused: time.Millisecond}, nil
}

// RestoreCheckpoint records the checkpoint restored and hands back a fresh
// instance carrying the captured filesystem, as a real engine does when the
// machine is replaced with one booted off the capture's disk.
func (f *Fake) RestoreCheckpoint(_ context.Context, _ rt.Instance, src rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	f.mu.Lock()
	f.restored = append(f.restored, spec.SandboxID+"/"+src.Name)
	f.mu.Unlock()
	inst, err := f.Boot(context.Background(), spec)
	if err != nil {
		return nil, err
	}
	snap, err := os.ReadFile(filepath.Join(src.Path, "files.json"))
	if err != nil {
		// A capture written before this existed, or by another engine: the
		// machine comes back empty rather than the restore failing.
		return inst, nil
	}
	files := map[string][]byte{}
	if err := json.Unmarshal(snap, &files); err != nil {
		return nil, fmt.Errorf("restore checkpoint: corrupt file snapshot: %w", err)
	}
	i := inst.(*fakeInstance)
	i.mu.Lock()
	i.files = files
	i.mu.Unlock()
	return inst, nil
}

// BootFromSnapshot implements rt.SnapshotBooter: a new sandbox out of a
// capture directory, recording which directory it was pointed at.
func (f *Fake) BootFromSnapshot(ctx context.Context, src rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, string, error) {
	f.mu.Lock()
	f.snapshotBooted = append(f.snapshotBooted, spec.SandboxID+"@"+src.Path)
	f.mu.Unlock()
	kind := f.SnapshotBootKind
	if kind == "" {
		kind = "memory"
	}
	inst, err := f.Boot(ctx, spec)
	if err != nil {
		return nil, "", err
	}
	return inst, kind, nil
}

var _ rt.SnapshotBooter = (*Fake)(nil)

// SnapshotBoots returns every BootFromSnapshot as "<sandbox>@<path>".
func (f *Fake) SnapshotBoots() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.snapshotBooted...)
}

// CheckpointRestores returns "<sandbox>/<name>" for every RestoreCheckpoint call.
func (f *Fake) CheckpointRestores() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.restored...)
}

func (f *Fake) RecoverOrphans() []rt.Instance { return nil }

// ExecLog returns the commands executed against the given sandbox.
func (f *Fake) ExecLog(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst, ok := f.instances[id]; ok {
		return append([]string(nil), inst.execLog...)
	}
	return nil
}

// ServerLog returns the StartServer commands issued to the sandbox's current
// instance, so after a Restore it reflects post-resume restarts only.
func (f *Fake) ServerLog(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst, ok := f.instances[id]; ok {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		return append([]string(nil), inst.servers...)
	}
	return nil
}

// LastServerEnv returns the env from the most recent StartServer, or nil.
func (f *Fake) LastServerEnv(id string) map[string]string {
	f.mu.Lock()
	inst, ok := f.instances[id]
	f.mu.Unlock()
	if !ok {
		return nil
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if len(inst.serverEnvs) == 0 {
		return nil
	}
	return inst.serverEnvs[len(inst.serverEnvs)-1]
}

type fakeInstance struct {
	// rtF is the runtime that built this instance, for the shared network table.
	rtF        *Fake
	id         string
	mu         sync.Mutex
	files      map[string][]byte
	execLog    []string
	servers    []string
	serverEnvs []map[string]string
	start      time.Time
	stopped    bool
	oomKills   int // test-settable count reported by OOMKillCount
	frozen     bool
	freezes    int
	thaws      int
	// fsWedged makes FS.Read block until the caller's ctx expires, like a
	// black-holed guest.
	fsWedged bool
}

// SetNetworks implements rt.Networker at the instance level, which is where a
// runtime keeps its id mapping.
func (i *fakeInstance) SetNetworks(_ context.Context, n rt.Networks) (string, error) {
	if i.rtF == nil {
		return "", rt.ErrUnsupported
	}
	return i.rtF.setNetworks(i.id, n)
}

// Stopped reports whether the given sandbox's instance received Stop.
func (f *Fake) Stopped(id string) bool {
	f.mu.Lock()
	inst, ok := f.instances[id]
	f.mu.Unlock()
	if !ok {
		return false
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.stopped
}

// WedgeFS marks a sandbox's instance as unable to answer FS calls.
func (f *Fake) WedgeFS(id string) {
	f.mu.Lock()
	inst, ok := f.instances[id]
	f.mu.Unlock()
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.fsWedged = true
	inst.mu.Unlock()
}

// SetOOMKills sets the count the sandbox's instance reports via rt.OOMReporter.
func (f *Fake) SetOOMKills(id string, n int) {
	f.mu.Lock()
	inst, ok := f.instances[id]
	f.mu.Unlock()
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.oomKills = n
	inst.mu.Unlock()
}

func (i *fakeInstance) ID() string { return i.id }

// Freeze and Thaw implement rt.Freezer, recording the calls so a test can tell a
// standby from a snapshot.
func (i *fakeInstance) Freeze(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.frozen = true
	i.freezes++
	return nil
}

func (i *fakeInstance) Thaw(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.frozen = false
	i.thaws++
	return nil
}

// Frozen reports whether the sandbox's instance is currently frozen, and how
// many times it has been frozen and thawed.
func (f *Fake) Frozen(id string) (frozen bool, freezes, thaws int) {
	f.mu.Lock()
	inst, ok := f.instances[id]
	f.mu.Unlock()
	if !ok {
		return false, 0, 0
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.frozen, inst.freezes, inst.thaws
}

// OOMKillCount implements rt.OOMReporter.
func (i *fakeInstance) OOMKillCount(context.Context) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.oomKills, nil
}

func (i *fakeInstance) Exec(_ context.Context, s rt.ExecSpec) (rt.ExecResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.execLog = append(i.execLog, s.Cmd)
	if s.Detach {
		return rt.ExecResult{Pid: 4242}, nil
	}
	return rt.ExecResult{Stdout: "", ExitCode: 0}, nil
}

func (i *fakeInstance) StartServer(_ context.Context, cmd string, env map[string]string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.servers = append(i.servers, cmd)
	i.serverEnvs = append(i.serverEnvs, env)
	return nil
}

func (i *fakeInstance) FS() rt.FileSystem { return (*fakeFS)(i) }

func (i *fakeInstance) Procs() ([]rt.Proc, error) { return []rt.Proc{{PID: 1, Cmd: "init"}}, nil }
func (i *fakeInstance) Signal(int, int) error     { return nil }
func (i *fakeInstance) ListenTarget(port int) (string, error) {
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}
func (i *fakeInstance) TerminalTarget() (string, error) {
	return "127.0.0.1:7681", nil
}
func (i *fakeInstance) Snapshot(context.Context, rt.SnapshotRef) error { return nil }

// Stop fails on a canceled ctx, like a real teardown, so a Destroy that forgets
// to detach from the caller's ctx is caught.
func (i *fakeInstance) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.stopped = true
	return nil
}
func (i *fakeInstance) Metrics() rt.InstanceMetrics {
	return rt.InstanceMetrics{VCPU: 1, MemMiB: 512, UptimeSecs: time.Since(i.start).Seconds()}
}

type fakeFS fakeInstance

func (f *fakeFS) Read(ctx context.Context, p string) ([]byte, error) {
	f.mu.Lock()
	wedged := f.fsWedged
	f.mu.Unlock()
	if wedged {
		<-ctx.Done() // only the caller's deadline ends the wait
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	if !ok {
		return nil, fmt.Errorf("no such file: %s", p)
	}
	return append([]byte(nil), b...), nil
}
func (f *fakeFS) Write(ctx context.Context, p string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = append([]byte(nil), data...)
	return nil
}
func (f *fakeFS) List(ctx context.Context, _ string) ([]rt.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.files))
	for n := range f.files {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]rt.DirEntry, 0, len(names))
	for _, n := range names {
		out = append(out, rt.DirEntry{Name: n, Size: int64(len(f.files[n]))})
	}
	return out, nil
}
func (f *fakeFS) Mkdir(ctx context.Context, _ string) error { return nil }
func (f *fakeFS) Move(ctx context.Context, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[src]
	if !ok {
		return fmt.Errorf("no such file: %s", src)
	}
	f.files[dst] = b
	delete(f.files, src)
	return nil
}
func (f *fakeFS) Remove(ctx context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}
func (f *fakeFS) Stat(ctx context.Context, p string) (rt.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	if !ok {
		return rt.FileInfo{}, fmt.Errorf("no such file: %s", p)
	}
	return rt.FileInfo{Name: p, Size: int64(len(b)), Mode: "-rw-r--r--"}, nil
}
