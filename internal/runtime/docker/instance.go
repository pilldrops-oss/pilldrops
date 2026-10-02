package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/envdproto"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// readyProbeClient probes a freshly started app. DisableKeepAlives is required:
// a pooled connection to Docker's port proxy can report ready before the
// backend binds.
var readyProbeClient = &http.Client{
	Timeout:   800 * time.Millisecond,
	Transport: &http.Transport{DisableKeepAlives: true},
}

// serverReadyTimeout bounds how long StartServer polls for an HTTP response.
// A plain dial is a false positive because Docker's port proxy accepts before
// the app binds. Timing out is not an error.
var serverReadyTimeout = 15 * time.Second

// dockerInstance is one sandbox backed by a running container. host and
// termHost are the published 127.0.0.1 addresses of the app and of envd's
// terminal.
type dockerInstance struct {
	id       string
	cid      string
	host     string
	termHost string
	// envdToken is the node's credential; only its hash entered the container.
	envdToken string
	vcpu      int
	memMiB    int
	service   bool // image-backed service (no /workspace) → don't force -w on exec
	// runUser is the identity every tenant-reachable operation runs as; ""
	// means uid 0 (see rootReason).
	runUser string
	start   time.Time
	execs   int64
	// eng is nil when the Engine socket was unreachable; every path then uses the CLI.
	eng *engine
	// primary is the port previews default to: appPort, or a service's first.
	primary int
	// ports caches `docker port` answers, seeded with the primary port; a
	// mapping is fixed for the container's life, so one lookup per port suffices.
	portsMu sync.Mutex
	ports   map[int]string
	// oomKilled is what the inspect that found the container dead said.
	oomKilled atomic.Bool
	// deadSince is the first of the consecutive reads that found it stopped.
	deadMu    sync.Mutex
	deadSince time.Time
	// casualty is what the last Alive read said: stopped by a dockerd restart.
	casualty atomic.Bool
}

func (i *dockerInstance) ID() string { return i.id }

func (i *dockerInstance) Exec(ctx context.Context, s rt.ExecSpec) (rt.ExecResult, error) {
	atomic.AddInt64(&i.execs, 1)
	ctx, cancel := context.WithTimeout(ctx, execTimeout(s))
	defer cancel()

	cmd, args := i.execCommand(s)
	so, se, code, err := i.runExec(ctx, s, cmd, args)
	if err != nil {
		return rt.ExecResult{Stdout: so, Stderr: se}, err
	}
	if s.Detach {
		pid, _ := strconv.Atoi(strings.TrimSpace(so))
		return rt.ExecResult{Pid: pid}, nil
	}
	return rt.ExecResult{Stdout: so, Stderr: se, ExitCode: code}, nil
}

// ExecStream implements rt.StreamExecutor: output reaches onFrame as the
// container writes it, not once the command exits. Without it a long-running
// command (a file watch, a build log) delivered nothing until it ended.
func (i *dockerInstance) ExecStream(ctx context.Context, s rt.ExecSpec, onFrame func(rt.StreamFrame)) error {
	if s.Detach {
		res, err := i.Exec(ctx, s)
		if err != nil {
			return err
		}
		onFrame(rt.StreamFrame{Stdout: []byte(strconv.Itoa(res.Pid)), Exited: true})
		return nil
	}
	atomic.AddInt64(&i.execs, 1)
	ctx, cancel := context.WithTimeout(ctx, execTimeout(s))
	defer cancel()

	cmd, args := i.execCommand(s)
	var mu sync.Mutex // the CLI path copies stdout and stderr on separate goroutines
	stdout := frameWriter{mu: &mu, emit: func(b []byte) { onFrame(rt.StreamFrame{Stdout: b}) }}
	stderr := frameWriter{mu: &mu, emit: func(b []byte) { onFrame(rt.StreamFrame{Stderr: b}) }}

	var code int
	var err error
	if i.eng != nil {
		var stdin io.Reader
		if len(s.Stdin) > 0 {
			stdin = bytes.NewReader(s.Stdin)
		}
		code, err = i.eng.ExecTo(ctx, i.cid, i.execConfig(s, cmd), stdin, stdout, stderr)
	} else {
		c := exec.CommandContext(ctx, "docker", args...)
		c.Stdout, c.Stderr = stdout, stderr
		err = c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code, err = ee.ExitCode(), nil
		}
	}
	if err != nil {
		return err
	}
	onFrame(rt.StreamFrame{ExitCode: code, Exited: true})
	return nil
}

var _ rt.StreamExecutor = (*dockerInstance)(nil)

// frameWriter hands each write to emit as its own frame, one at a time.
type frameWriter struct {
	mu   *sync.Mutex
	emit func([]byte)
}

func (f frameWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emit(append([]byte(nil), b...))
	return len(b), nil
}

// execTimeout is an exec's deadline: its own, or 30 s.
func execTimeout(s rt.ExecSpec) time.Duration {
	if s.Timeout == 0 {
		return 30 * time.Second
	}
	return s.Timeout
}

// execCommand builds the shell command and the `docker exec` argv for s. The
// Engine API path runs the same command, so the two cannot drift.
func (i *dockerInstance) execCommand(s rt.ExecSpec) (string, []string) {
	// Service containers have no /workspace; exec in the image's own workdir.
	var args []string
	if i.service {
		if s.Cwd != "" {
			args = []string{"exec", "-w", path.Clean("/" + s.Cwd)}
		} else {
			args = []string{"exec"}
		}
	} else {
		wd := containerWorkdir
		if s.Cwd != "" {
			wd = path.Join(containerWorkdir, path.Clean("/"+s.Cwd))
		}
		args = []string{"exec", "-w", wd}
	}
	args = append(args, i.userArgs()...)
	// Before the request env so an explicit PORT wins via docker's last -e.
	if !i.service {
		args = append(args, "-e", "PORT="+strconv.Itoa(appPort))
	}
	for k, v := range s.Env {
		args = append(args, "-e", k+"="+v)
	}
	cmd := s.Cmd
	if s.Detach {
		cmd = "{ " + s.Cmd + " ; } >/tmp/pd-detach.log 2>&1 & echo $!"
	}
	// Outside the detach wrapper so the backgrounded command inherits PATH too.
	cmd = identityEnvPrefix + execPathPrefix(i.runUser) + cmd
	return cmd, append(args, i.cid, "/bin/sh", "-c", cmd)
}

// runExec dispatches one exec to the Engine API when available, else the CLI.
// The caller builds the CLI argv either way so the fallback cannot drift.
func (i *dockerInstance) runExec(ctx context.Context, spec rt.ExecSpec, cmd string, cliArgs []string) (string, string, int, error) {
	if i.eng == nil {
		return dockerOut(ctx, cliArgs...)
	}
	var stdin io.Reader
	if len(spec.Stdin) > 0 {
		stdin = bytes.NewReader(spec.Stdin)
	}
	so, se, code, err := i.eng.Exec(ctx, i.cid, i.execConfig(spec, cmd), stdin)
	if err != nil {
		// Transport failure only; a non-zero exit returns err == nil and is not retried.
		// ponytail: the CLI fallback carries no stdin; the Engine API is the primary path.
		return dockerOut(ctx, cliArgs...)
	}
	return so, se, code, nil
}

// execConfig is the Engine API form of the command execCommand builds.
func (i *dockerInstance) execConfig(spec rt.ExecSpec, cmd string) execConfig {
	cfg := execConfig{Cmd: []string{"/bin/sh", "-c", cmd}, User: i.runUser}
	if i.service {
		if spec.Cwd != "" {
			cfg.WorkingDir = path.Clean("/" + spec.Cwd)
		}
	} else {
		cfg.WorkingDir = containerWorkdir
		if spec.Cwd != "" {
			cfg.WorkingDir = path.Join(containerWorkdir, path.Clean("/"+spec.Cwd))
		}
		cfg.Env = append(cfg.Env, "PORT="+strconv.Itoa(appPort))
	}
	for k, v := range spec.Env {
		cfg.Env = append(cfg.Env, k+"="+v)
	}
	return cfg
}

// DiskUsageMB reports the larger of the writable layer size and du of
// /workspace. Neither alone survives pause/resume: a resumed container boots
// from the committed image, so its layer size resets to zero.
func (i *dockerInstance) DiskUsageMB(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var (
		layerBytes int64
		err        error
	)
	if i.eng != nil {
		layerBytes, err = i.eng.InspectSize(ctx, i.cid)
	}
	if i.eng == nil || err != nil {
		out, _, code, cerr := dockerOut(ctx, "inspect", "--size", "--format", "{{.SizeRw}}", i.cid)
		if cerr != nil || code != 0 {
			return 0, fmt.Errorf("docker inspect --size: %w", cerr)
		}
		layerBytes, err = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse SizeRw %q: %w", strings.TrimSpace(out), err)
		}
	}
	mb := int(layerBytes / (1024 * 1024))

	// Best-effort; -x keeps the walk off mounted volumes.
	if duOut, _, duCode, duErr := dockerOut(ctx, "exec", i.cid, "du", "-sxm", containerWorkdir); duErr == nil && duCode == 0 {
		if f := strings.Fields(duOut); len(f) > 0 {
			if wsMB, perr := strconv.Atoi(f[0]); perr == nil && wsMB > mb {
				mb = wsMB
			}
		}
	}
	return mb, nil
}

// userArgs is the `docker exec` flag pair for the run user, or nil for root.
func (i *dockerInstance) userArgs() []string {
	if i.runUser == "" {
		return nil
	}
	return []string{"--user", i.runUser}
}

// FS runs as the same identity as exec, or uploads land root-owned where the
// tenant shell cannot overwrite them.
func (i *dockerInstance) FS() rt.FileSystem {
	return &dockerFS{cid: i.cid, eng: i.eng, user: i.runUser}
}

// isKeepalive defines ka, true for the pid that is the container's keepalive:
// the `sleep infinity` PID 1 (docker-init) runs, whose exit ends the container.
// It is told from a tenant's orphaned `sleep infinity`, also PID 1's child, by
// its start time (stat field 22): docker-init forked it at once, so it started
// within a second (100 ticks) of PID 1. Fork time, so the shell's exec into
// sleep after the workspace chown does not move it.
const isKeepalive = `st() { set -- $(cat /proc/$1/stat 2>/dev/null); eval echo "\${22:-0}"; }; ` +
	`ka() { [ "$(tr '\0' ' ' < /proc/$1/cmdline 2>/dev/null)" = "sleep infinity " ] && ` +
	`grep -qx 'PPid:[[:space:]]*1' /proc/$1/status 2>/dev/null && ` +
	`[ "$(st "$1")" -le $(( $(st 1) + 100 )) ]; }; `

// procList prints "pid comm" for each process in the container's own pid
// namespace, skipping itself and the keepalive. docker top reports host pids,
// which a kill run inside the container (Signal, or the tenant's own shell)
// cannot address.
const procList = isKeepalive + `for d in /proc/[0-9]*; do p=${d#/proc/}; [ "$p" = $$ ] && continue; ` +
	`{ read -r c < "$d/comm"; } 2>/dev/null || continue; [ "$c" = sleep ] && ka "$p" && continue; ` +
	`echo "$p $c"; done`

// signalScript kills $2 with signal $1 unless it is the keepalive, for which
// it exits keepaliveExit.
const (
	signalScript  = isKeepalive + `ka "$2" && exit 64; kill -"$1" "$2"`
	keepaliveExit = 64
)

// Alive implements rt.Liveness. An inspect that fails for any reason but a
// missing container answers alive: reaping on a dockerd hiccup would destroy
// every sandbox on the node.
func (i *dockerInstance) Alive() bool {
	st, err := i.state()
	if err != nil && !strings.Contains(err.Error(), "No such") { // engine: "container", CLI: "object"
		return true
	}
	if err == nil && st.Running {
		i.deadMu.Lock()
		i.deadSince = time.Time{}
		i.deadMu.Unlock()
		return true
	}
	i.oomKilled.Store(st.OOMKilled)
	// A dockerd restart stopped it: not dead, and the daemon revives it.
	casualty := restartCasualty(st.FinishedAt, dockerdStarted())
	i.casualty.Store(casualty)
	return !casualty && !i.confirmDead(time.Now())
}

// Casualty implements rt.Reviver.
func (i *dockerInstance) Casualty() bool { return i.casualty.Load() }

// EngineStarted implements rt.Reviver.
func (*dockerInstance) EngineStarted() time.Time { return dockerdStarted() }

// reviveTries and reviveBackoff bound the docker start of a restart casualty:
// the new daemon can still be settling when the first one runs.
var (
	reviveTries   = 3
	reviveBackoff = 2 * time.Second
)

// Revive implements rt.Reviver: start the container again and re-read its
// published ports, which docker reallocates on a start, as Adopt does.
func (i *dockerInstance) Revive(ctx context.Context) (rt.Instance, error) {
	err := retryStart(ctx, func() error {
		_, se, code, err := dockerOut(ctx, "start", i.cid)
		if err == nil && code != 0 {
			err = errors.New(strings.TrimSpace(se))
		}
		return err
	})
	if err == nil {
		err = stayedUp(ctx, reviveSettle, i.state)
	}
	if err != nil {
		return nil, fmt.Errorf("docker start %s: %w", i.cid, err)
	}
	host, _ := mappedPort(ctx, i.cid, i.primary)
	termHost, _ := mappedPort(ctx, i.cid, terminalPort)
	return &dockerInstance{
		id: i.id, cid: i.cid, host: host, termHost: termHost, envdToken: i.envdToken,
		vcpu: i.vcpu, memMiB: i.memMiB, service: i.service, runUser: i.runUser,
		start: time.Now(), eng: i.eng, primary: i.primary, ports: map[int]string{i.primary: host},
	}, nil
}

// reviveSettle is how long a started container must stay running to count as
// revived: `docker start` exits 0 for one whose command dies at once.
const reviveSettle = 2 * time.Second

// stayedUp waits settle, then confirms the container is still running. A
// state read that fails confirms nothing either way and is not a failure.
func stayedUp(ctx context.Context, settle time.Duration, state func() (containerState, error)) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
	}
	if st, err := state(); err == nil && !st.Running {
		return fmt.Errorf("%w: exited right after start", rt.ErrWontStayUp)
	}
	return nil
}

// retryStart runs start up to reviveTries times, reviveBackoff apart.
func retryStart(ctx context.Context, start func() error) error {
	var err error
	for n := range reviveTries {
		if n > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reviveBackoff):
			}
		}
		if err = start(); err == nil {
			return nil
		}
	}
	return err
}

func (i *dockerInstance) state() (containerState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if i.eng != nil {
		return i.eng.State(ctx, i.cid)
	}
	out, se, code, err := dockerOut(ctx, "inspect", "-f",
		"{{.State.Running}} {{.State.OOMKilled}} {{.State.FinishedAt}}", i.cid)
	if err == nil && code != 0 {
		err = errors.New(se)
	}
	if err != nil {
		return containerState{}, err
	}
	var st containerState
	if f := strings.Fields(out); len(f) == 3 {
		st.Running, st.OOMKilled = f[0] == "true", f[1] == "true"
		st.FinishedAt, _ = time.Parse(time.RFC3339Nano, f[2])
	}
	return st, nil
}

// deadConfirmAfter is how long a container must stay stopped, across two
// reads, before it is reported dead; a var so tests can shrink it.
var deadConfirmAfter = 5 * time.Second

// restartGrace is how long after dockerd's start a stop still counts as that
// restart's doing: after a crash the new daemon stops the orphaned containers
// just after it starts. A stop before its start, however long ago, was the old
// daemon going away under the container.
const restartGrace = 60 * time.Second

// confirmDead decides whether a container read as stopped at now is dead: it
// must stay stopped across two reads deadConfirmAfter apart.
func (i *dockerInstance) confirmDead(now time.Time) bool {
	i.deadMu.Lock()
	defer i.deadMu.Unlock()
	if i.deadSince.IsZero() {
		i.deadSince = now
		return false
	}
	return now.Sub(i.deadSince) >= deadConfirmAfter
}

// restartCasualty reports whether a stop at finishedAt was the doing of the
// dockerd that started at dockerdStart. An unknown start or stop time is never
// a casualty: the container is then judged by confirmDead like any exit.
func restartCasualty(finishedAt, dockerdStart time.Time) bool {
	if finishedAt.IsZero() || dockerdStart.IsZero() {
		return false
	}
	return finishedAt.Before(dockerdStart.Add(restartGrace))
}

// OOMKilled implements rt.OOMKilledReporter.
func (i *dockerInstance) OOMKilled() bool { return i.oomKilled.Load() }

func (i *dockerInstance) Procs() ([]rt.Proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, _, code, err := i.runExec(ctx, rt.ExecSpec{}, procList, []string{"exec", i.cid, "/bin/sh", "-c", procList}); err == nil && code == 0 {
		return parseProcs(out, false), nil
	}
	// An image with no /bin/sh (a distroless service): docker top still lists
	// it, but by host pids, which a kill inside the container would read as
	// some other process and which say nothing a tenant should see. Listed with
	// pid 0, which the signal route refuses.
	out, _, code, err := dockerOut(ctx, "top", i.cid, "-eo", "pid,comm")
	if err != nil || code != 0 {
		return nil, nil // best-effort, like the process runtime
	}
	procs := parseProcs(out, true)
	for n := range procs {
		procs[n].PID = 0
	}
	return procs, nil
}

// parseProcs reads "pid comm" lines, skipping docker top's header row.
func parseProcs(out string, header bool) []rt.Proc {
	var procs []rt.Proc
	for n, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if header && n == 0 {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if pid, perr := strconv.Atoi(f[0]); perr == nil {
			procs = append(procs, rt.Proc{PID: pid, Cmd: strings.Join(f[1:], " ")})
		}
	}
	return procs
}

func (i *dockerInstance) Signal(pid, sig int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"exec", i.cid, "kill", "-" + strconv.Itoa(sig), strconv.Itoa(pid)}
	if !i.service { // only the platform's own command has a keepalive
		args = []string{"exec", i.cid, "sh", "-c", signalScript, "sh", strconv.Itoa(sig), strconv.Itoa(pid)}
	}
	_, se, code, err := dockerOut(ctx, args...)
	if err != nil {
		return fmt.Errorf("signal %d/%d: %w", pid, sig, err)
	}
	if !i.service && code == keepaliveExit {
		return fmt.Errorf("pid %d: %w", pid, rt.ErrProtectedProcess)
	}
	if code != 0 {
		return fmt.Errorf("signal %d/%d: %s", pid, sig, strings.TrimSpace(se))
	}
	return nil
}

// OOMKillCount reads the container's own cgroup-v2 memory.events counter;
// dmesg would leak host-wide messages.
func (i *dockerInstance) OOMKillCount(ctx context.Context) (int, error) {
	out, _, _, err := dockerOut(ctx, "exec", i.cid, "sh", "-c",
		`awk '/^oom_kill /{print $2}' /sys/fs/cgroup/memory.events 2>/dev/null`)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n, nil
}

// ListenTarget resolves a container port to its published host address.
func (i *dockerInstance) ListenTarget(port int) (string, error) {
	if port == rt.TerminalPort && i.termHost != "" {
		return i.termHost, nil
	}
	if i.host == "" {
		return "", nil
	}
	// The primary port is seeded at boot/adopt. Any other port must have been
	// published then too (a declared template port, boot.go); docker answers
	// for those and fails for the rest, so an undeclared port is "no app"
	// rather than the primary port's mapping.
	i.portsMu.Lock()
	defer i.portsMu.Unlock()
	if h, ok := i.ports[port]; ok {
		return h, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := mappedPort(ctx, i.cid, port)
	if err != nil {
		return "", fmt.Errorf("%w: sandbox %s publishes port %d and its declared ports only, not %d; docker "+
			"publishes ports when the container is created, so create the sandbox with \"ports\":[%d]",
			rt.ErrNoAppOnPort, i.id, appPort, port, port)
	}
	if i.ports == nil {
		i.ports = map[int]string{}
	}
	i.ports[port] = h
	return h, nil
}

// TerminalTarget returns the published host address of envd's web terminal.
func (i *dockerInstance) TerminalTarget() (string, error) {
	if i.termHost == "" {
		return "", fmt.Errorf("sandbox %s has no published terminal port", i.id)
	}
	return i.termHost, nil
}

// EnvdProtocol implements rt.EnvdProtocoler. A container runs the node's own
// envd, bind-mounted at boot, which ships with this binary.
func (i *dockerInstance) EnvdProtocol() int { return envdproto.Protocol }

// TerminalToken returns the credential envd's web terminal requires; empty
// when envd runs unauthenticated.
func (i *dockerInstance) TerminalToken() (string, error) {
	return i.envdToken, nil
}

// StartServer launches the app command detached inside the container with PORT
// and env injected, appending combined output to AppLogPath.
func (i *dockerInstance) StartServer(ctx context.Context, cmd string, env map[string]string) error {
	logPath := path.Join(containerWorkdir, rt.AppLogPath)
	wrapped := identityEnvPrefix + execPathPrefix(i.runUser) +
		fmt.Sprintf("mkdir -p %q; { %s ; } >> %q 2>&1", path.Dir(logPath), cmd, logPath)
	args := []string{"exec", "-d", "-w", containerWorkdir}
	args = append(args, i.userArgs()...)
	if _, ok := env["PORT"]; !ok {
		args = append(args, "-e", "PORT="+strconv.Itoa(appPort))
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, i.cid, "/bin/sh", "-c", wrapped)
	_, se, code, err := dockerOut(ctx, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("start server: %w %s", err, strings.TrimSpace(se))
	}
	i.waitHTTPReady(ctx)
	return nil
}

// waitHTTPReady polls the app with real GETs until any HTTP response arrives
// or serverReadyTimeout elapses.
func (i *dockerInstance) waitHTTPReady(ctx context.Context) {
	if i.host == "" {
		return
	}
	deadline := time.Now().Add(serverReadyTimeout)
	url := "http://" + i.host + "/"
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := readyProbeClient.Do(req)
		if err == nil {
			resp.Body.Close()
			return // any HTTP status means the backend is up
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// Snapshot commits the whole container filesystem to a local image, so packages
// installed outside /workspace survive resume, and writes the tag to dst.Path.
func (i *dockerInstance) Snapshot(ctx context.Context, dst rt.SnapshotRef) error {
	tag := snapImageTag(i.id)
	if _, se, code, err := dockerOut(ctx, "commit", i.cid, tag); err != nil || code != 0 {
		return fmt.Errorf("commit snapshot: %w %s", err, strings.TrimSpace(se))
	}
	return os.WriteFile(dst.Path, []byte(tag), 0o600)
}

func (i *dockerInstance) Stop(ctx context.Context) error {
	_, se, code, err := dockerOut(ctx, "rm", "-f", i.cid)
	if err != nil || code != 0 {
		return fmt.Errorf("remove container: %w %s", err, strings.TrimSpace(se))
	}
	return nil
}

func (i *dockerInstance) Metrics() rt.InstanceMetrics {
	return rt.InstanceMetrics{
		VCPU: i.vcpu, MemMiB: i.memMiB,
		ExecCount: atomic.LoadInt64(&i.execs), UptimeSecs: time.Since(i.start).Seconds(),
	}
}

// LiveStats samples CPU%, resident memory and workspace disk usage.
// DiskUsedMB is -1 when du is unavailable.
func (i *dockerInstance) LiveStats(ctx context.Context) (rt.LiveStats, error) {
	ls := rt.LiveStats{DiskUsedMB: -1}
	sctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	// e.g. "12.34%|187.3MiB / 2GiB"
	so, se, code, err := dockerOut(sctx, "stats", "--no-stream", "--format", "{{.CPUPerc}}|{{.MemUsage}}", i.cid)
	if err != nil || code != 0 {
		return ls, fmt.Errorf("docker stats: %w %s", err, strings.TrimSpace(se))
	}
	cpuStr, memStr, _ := strings.Cut(strings.TrimSpace(so), "|")
	if v, e := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(cpuStr), "%"), 64); e == nil {
		ls.CPUPct = v
	}
	if used, _, ok := strings.Cut(memStr, "/"); ok {
		ls.MemUsedMiB = parseMiB(strings.TrimSpace(used))
	}

	if !i.service {
		if do, _, dcode, derr := dockerOut(sctx, "exec", i.cid, "du", "-sm", containerWorkdir); derr == nil && dcode == 0 {
			if f := strings.Fields(strings.TrimSpace(do)); len(f) > 0 {
				if mb, e := strconv.Atoi(f[0]); e == nil {
					ls.DiskUsedMB = mb
				}
			}
		}
	}
	return ls, nil
}

// parseMiB converts a docker-formatted size ("187.3MiB", "2.1GiB") to whole MiB.
func parseMiB(s string) int {
	s = strings.TrimSpace(s)
	var mult float64
	switch {
	case strings.HasSuffix(s, "GiB"):
		mult, s = 1024, strings.TrimSuffix(s, "GiB")
	case strings.HasSuffix(s, "MiB"):
		mult, s = 1, strings.TrimSuffix(s, "MiB")
	case strings.HasSuffix(s, "KiB"):
		mult, s = 1.0/1024, strings.TrimSuffix(s, "KiB")
	case strings.HasSuffix(s, "B"):
		mult, s = 1.0/(1024*1024), strings.TrimSuffix(s, "B")
	default:
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int(v * mult)
}

var _ rt.Instance = (*dockerInstance)(nil)
var _ rt.LiveStatser = (*dockerInstance)(nil)
