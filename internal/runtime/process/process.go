package process

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Runtime creates process-backed instances rooted under baseDir.
type Runtime struct {
	baseDir string
}

// New returns a process Runtime storing sandbox workdirs under baseDir. It
// panics if baseDir cannot be created, since every Boot would fail anyway.
func New(baseDir string) *Runtime {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		panic(fmt.Sprintf("process runtime: cannot create base dir %q: %v", baseDir, err))
	}
	return &Runtime{baseDir: baseDir}
}

func (r *Runtime) Name() string          { return "process" }
func (r *Runtime) Capabilities() rt.Caps { return DeclaredCaps() }

func (r *Runtime) Prepare(context.Context, rt.Template) error { return nil }

func (r *Runtime) Boot(_ context.Context, spec rt.BootSpec) (rt.Instance, error) {
	workDir := filepath.Join(r.baseDir, spec.SandboxID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	return &processInstance{
		id: spec.SandboxID, workDir: workDir, vcpu: spec.VCPU, memMiB: spec.MemMiB,
		appPort: port, start: time.Now(),
		bootEnv: append([]string(nil), spec.Template.Env...),
	}, nil
}

// freePort asks the OS for an unused loopback port. The race between closing
// the probe and the app binding is acceptable for a dev runtime.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close() //nolint:errcheck // best-effort: freeing the probe listener, nothing to react to
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (r *Runtime) Restore(ctx context.Context, snap rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	inst, err := r.Boot(ctx, spec)
	if err != nil {
		return nil, err
	}
	pi := inst.(*processInstance)
	if !snap.IsZero() {
		if err := untar(snap.Path, pi.workDir); err != nil {
			return nil, err
		}
	}
	return pi, nil
}

func (r *Runtime) RecoverOrphans() []rt.Instance { return nil }

// processInstance is one sandbox: a workdir plus a paused flag.
type processInstance struct {
	id      string
	workDir string
	vcpu    int
	memMiB  int
	start   time.Time

	// bootEnv is the sandbox's create-time env ("NAME=VALUE").
	bootEnv []string

	mu      sync.Mutex
	paused  bool
	execs   int64
	appPort int       // loopback port assigned at boot; apps bind it via $PORT
	server  *exec.Cmd // the long-lived StartServer process, if any
}

func (i *processInstance) ID() string { return i.id }

// execEnv builds a command's environment: host env, HOME and PORT, bootEnv,
// then the per-call env. Last assignment wins, matching firecracker's withEnv.
func (i *processInstance) execEnv(reqEnv map[string]string) []string {
	env := append(os.Environ(), "HOME="+i.workDir, fmt.Sprintf("PORT=%d", i.appPort))
	env = append(env, i.bootEnv...)
	for k, v := range reqEnv {
		env = append(env, k+"="+v)
	}
	return env
}

func (i *processInstance) Exec(ctx context.Context, s rt.ExecSpec) (rt.ExecResult, error) {
	i.mu.Lock()
	paused := i.paused
	i.mu.Unlock()
	if paused {
		return rt.ExecResult{}, rt.ErrPaused
	}
	atomic.AddInt64(&i.execs, 1)

	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwd := i.workDir
	if s.Cwd != "" {
		cwd = filepath.Join(i.workDir, s.Cwd)
	}
	if s.Detach {
		// A per-sandbox log, since sandboxes share /tmp here; inheriting the
		// parent's pipes would hold the RPC open.
		logPath := filepath.Join(i.workDir, "pd-detach.log")
		dc := exec.CommandContext(ctx, "/bin/sh", "-c",
			"{ "+s.Cmd+" ; } >"+logPath+" 2>&1 & echo $!")
		dc.Dir = cwd
		dc.Env = i.execEnv(s.Env)
		out, err := dc.Output()
		if err != nil {
			return rt.ExecResult{}, err
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return rt.ExecResult{}, fmt.Errorf("parse detached pid: %w", err)
		}
		return rt.ExecResult{Pid: pid}, nil
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", s.Cmd)
	cmd.Dir = cwd
	cmd.Env = i.execEnv(s.Env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := rt.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil // non-zero exit is a result, not an error
	}
	if err != nil {
		return res, err
	}
	res.ExitCode = 0
	return res, nil
}

func (i *processInstance) FS() rt.FileSystem { return &workdirFS{root: i.workDir} }

// DiskUsageMB is the size of the sandbox workdir.
func (i *processInstance) DiskUsageMB(_ context.Context) (int, error) {
	var total int64
	err := filepath.WalkDir(i.workDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // a file vanishing mid-walk is not an error
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(total / (1024 * 1024)), nil
}

func (i *processInstance) Procs() ([]rt.Proc, error) { return nil, nil }
func (i *processInstance) Signal(int, int) error     { return nil }

// StartServer runs a long-lived command bound to $PORT and returns once it has
// started. Output is appended to rt.AppLogPath in the sandbox.
func (i *processInstance) StartServer(_ context.Context, cmd string, env map[string]string) error {
	i.mu.Lock()
	if i.paused {
		i.mu.Unlock()
		return rt.ErrPaused
	}
	i.mu.Unlock()

	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = i.workDir
	// Own process group so Stop can kill the whole tree, not just the shell.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// PORT and HOME go last: a server must bind the assigned port or the preview proxy has nothing to dial.
	c.Env = append(i.execEnv(env),
		fmt.Sprintf("PORT=%d", i.appPort),
		"HOME="+i.workDir,
	)
	// Append, since resume replays the start command. A capture failure only loses logs.
	if err := os.MkdirAll(filepath.Join(i.workDir, filepath.Dir(rt.AppLogPath)), 0o755); err == nil {
		if f, err := os.OpenFile(filepath.Join(i.workDir, rt.AppLogPath),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			c.Stdout, c.Stderr = f, f
			defer f.Close() //nolint:errcheck // Start dups the fd; the process keeps writing after this closes
		}
	}
	if err := c.Start(); err != nil {
		return err
	}
	i.mu.Lock()
	i.server = c
	i.mu.Unlock()
	// Reap so it does not linger as a zombie; Stop kills the group directly.
	go func() {
		_ = c.Wait() //nolint:errcheck // best-effort reap
	}()
	return nil
}

// ListenTarget ignores the requested port: every sandbox has exactly one loopback port.
func (i *processInstance) ListenTarget(int) (string, error) {
	return fmt.Sprintf("127.0.0.1:%d", i.appPort), nil
}

// TerminalTarget points at the single shared envd's terminal port on loopback.
func (i *processInstance) TerminalTarget() (string, error) {
	return fmt.Sprintf("127.0.0.1:%d", processTerminalPort()), nil
}

// processTerminalPort reads PD_TERMINAL_PORT, falling back to rt.TerminalPort.
func processTerminalPort() int {
	if v := os.Getenv("PD_TERMINAL_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return rt.TerminalPort
}

// Pause marks the instance paused so Exec is rejected.
func (i *processInstance) Pause() {
	i.mu.Lock()
	i.paused = true
	i.mu.Unlock()
}

// Resume clears the paused flag.
func (i *processInstance) Resume() {
	i.mu.Lock()
	i.paused = false
	i.mu.Unlock()
}

func (i *processInstance) Snapshot(_ context.Context, dst rt.SnapshotRef) error {
	return tarDir(i.workDir, dst.Path)
}

func (i *processInstance) Stop(context.Context) error {
	i.mu.Lock()
	srv := i.server
	i.server = nil
	i.mu.Unlock()
	if srv != nil && srv.Process != nil {
		// Kill the process group (pgid == pid via Setpgid), not just the sh wrapper.
		if err := syscall.Kill(-srv.Process.Pid, syscall.SIGKILL); err != nil {
			_ = srv.Process.Kill() //nolint:errcheck // last-resort fallback; nothing further to try if this also fails
		}
	}
	return os.RemoveAll(i.workDir)
}

func (i *processInstance) Metrics() rt.InstanceMetrics {
	return rt.InstanceMetrics{
		VCPU: i.vcpu, MemMiB: i.memMiB,
		ExecCount: atomic.LoadInt64(&i.execs), UptimeSecs: time.Since(i.start).Seconds(),
	}
}

// tarDir writes dir into a tar archive at dst.
func tarDir(dir, dst string) (err error) {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	// Close errors can mean a truncated snapshot, so surface them unless the
	// walk already failed. tw.Close runs first and flushes the footer.
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(out)
	defer func() {
		if cerr := tw.Close(); err == nil {
			err = cerr
		}
	}()
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		hdr := &tar.Header{Name: rel, Mode: 0o644, Size: info.Size()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read-only fd; the copy below already surfaces read errors
		_, err = io.Copy(tw, f)
		return err
	})
	return err
}

// Caps on what untar will extract. Vars, not consts, so the bomb test can
// lower them instead of shipping a multi-GiB fixture.
var (
	maxUntarBytes   int64 = 4 << 30 // total across all entries
	maxUntarEntries       = 65536
)

func untar(src, dir string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only fd; nothing to flush
	tr := tar.NewReader(in)
	var total int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxUntarEntries {
			return fmt.Errorf("snapshot archive has more than %d entries", maxUntarEntries)
		}
		// An entry with an absolute name or ".." must not escape dir.
		target := filepath.Join(dir, filepath.Clean("/"+hdr.Name))
		if rel, rerr := filepath.Rel(dir, target); rerr != nil ||
			rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry %q escapes sandbox root", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		// LimitReader, not hdr.Size, because headers can lie. +1 makes crossing
		// the cap observable as total > max rather than a truncated file.
		n, err := io.Copy(f, io.LimitReader(tr, maxUntarBytes-total+1))
		total += n
		if err != nil {
			f.Close() //nolint:errcheck // best-effort cleanup; the copy error above is what's returned
			return err
		}
		if total > maxUntarBytes {
			f.Close() //nolint:errcheck // the archive is rejected; nothing to flush
			return fmt.Errorf("snapshot archive exceeds %d bytes extracted", maxUntarBytes)
		}
		// The Close error is the only signal the data reached disk.
		if err := f.Close(); err != nil {
			return err
		}
	}
}

var _ rt.Runtime = (*Runtime)(nil)
