// envd is the guest agent that runs inside every sandbox VM, built static:
// CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/envd ./cmd/envd/
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/pilldrops-oss/pilldrops/internal/envdproto"
	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

const (
	vsockPort = 6001
	// credVsockPort is the AF_VSOCK port the node pushes the token hash to.
	// Separate from the TCP data port; AF_VSOCK shares no port space with TCP.
	credVsockPort = 6002
	// version is what Health reports; the node reads its capability level from it.
	version   = envdproto.Tag
	maxOutput = 10 * 1024 * 1024 // hard cap so runaway output cannot OOM the guest
)

var transportLog onceLogger

func main() {
	unignoreInterrupt()

	// `envd watch <dir>` is a one-shot tool the node runs through exec, not a
	// second server; it must return before anything below binds or starts.
	if len(os.Args) > 1 && os.Args[1] == "watch" {
		os.Exit(runWatch(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == "reap" {
		reapForever()
	}

	// A fast-boot microVM has no entropy; without this Node and Python hang on getrandom().
	seedEntropy()

	// A snapshot resume flaps the link and drops the boot-time IP config.
	startNetHeal()

	// bash -l sources /etc/profile, which resets PATH after envd sets it.
	// Best-effort: a read-only /etc must not stop the control plane.
	if err := writeProfileDropIn(templateEnvDir, profileDir); err != nil {
		fmt.Fprintf(os.Stderr, "envd: profile drop-in: %v\n", err)
	}

	// A snapshot-restored guest has no boot hash and gets one over the credential channel.
	hashes := newHashStore(readTokenHash())

	// Not fatal: Docker has no vsock and a cold-booted guest already has its
	// hash from the cmdline. credErr is reported in the transport notice.
	_, credErr := startCredChannel(credVsockPort, hashes)

	network, addr, reason, err := envdListenAddr(vsockPort, hashes.Get(), credErr == nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envd: FATAL: %v\n", err)
		os.Exit(1)
	}
	transportLog.log(os.Stdout, transportNotice(network, reason, credVsockPort, credErr))

	// Shares the hash store so a credential pushed after boot also unlocks /ws.
	startTerminalServer(hashes, credErr == nil)

	// Docker delivers the create env as envd's own process env, so a desktop
	// asked for at create starts here. Firecracker pushes it after boot, and
	// SetEnv fires this again.
	maybeStartDesktop()

	listener, err := net.Listen(network, addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envd: listen error: %v\n", err)
		os.Exit(1)
	}

	// Auth has no off mode: without a hash every authenticated method is refused.
	srv := grpc.NewServer(tokenAuthOptions(hashes)...)
	if hashes.Get() == "" {
		fmt.Println("envd: no credential yet — every authenticated method is refused until one is pushed")
	} else {
		fmt.Println("envd: per-sandbox access-token auth ENABLED")
	}
	pb.RegisterEnvdServiceServer(srv, &envdServer{})
	fmt.Printf("envd %s listening on port %d\n", version, vsockPort)
	if err := srv.Serve(listener); err != nil {
		fmt.Fprintf(os.Stderr, "envd: serve error: %v\n", err)
		os.Exit(1)
	}
}

// unignoreInterrupt gives every child envd spawns a default SIGINT, so Ctrl-C
// reaches a terminal's foreground job. init starts envd as `envd &`, and sh
// sets SIGINT (and SIGQUIT) to SIG_IGN for a background job; an ignored signal
// survives exec, so every shell and command inherited it. The Go runtime keeps
// an inherited SIG_IGN for SIGINT (it already handles SIGQUIT itself), and a
// handled signal is reset to SIG_DFL by exec, so catching it here is enough.
// The buffered channel is never read: a stray SIGINT to envd itself is dropped.
func unignoreInterrupt() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGINT)
}

// envdListenAddr decides whether envd may bind its control-plane port and why.
// The port is on the shared bridge, so it is refused unless a boot hash or a live
// credential channel can authenticate callers. Never return vsock: the host dials TCP.
func envdListenAddr(vsockPort int, hash string, credChannel bool) (network, addr, reason string, err error) {
	switch {
	case hash != "":
		return "tcp", fmt.Sprintf(":%d", vsockPort), "boot provisioned a token hash", nil
	case credChannel:
		return "tcp", fmt.Sprintf(":%d", vsockPort),
			"no boot credential; a vsock credential channel is live and every authenticated method " +
				"is refused until the node pushes one", nil
	default:
		return "", "", "", errors.New(
			"refusing to bind a routable control plane with neither a token hash nor a vsock " +
				"credential channel: every sandbox on the shared network could otherwise drive this envd")
	}
}

type envdServer struct {
	pb.UnimplementedEnvdServiceServer
}

// Health reports that envd is fully initialized.
func (s *envdServer) Health(_ context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Ready: true, Version: version}, nil
}

// Execute runs a shell command and streams its output, capped, with the whole process group killed on timeout.
func (s *envdServer) Execute(req *pb.ExecuteRequest, stream pb.EnvdService_ExecuteServer) error {
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	cmd := exec.Command("/bin/sh", "-c", req.Cmd)

	// Own process group so kill(-pid) reaches children that would outlive the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	} else {
		cmd.Dir = "/root"
	}

	cmd.Env = buildSessionEnv(req.Env)
	// ponytail: whole stdin in one message (the node caps it at 64 MiB); a bidi stream if someone pipes more.
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	// Raw pipes instead of StdoutPipe so streaming can stop when the direct
	// child exits. A backgrounded grandchild (`cmd && nohup srv &`) inherits the
	// write end, and waiting for EOF would block until the deadline killed it.
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = stream.Send(&pb.ExecuteResponse{Type: pb.OutputType_EXIT, ExitCode: -1})
		return nil
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = stream.Send(&pb.ExecuteResponse{Type: pb.OutputType_EXIT, ExitCode: -1})
		return nil
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		_ = stream.Send(&pb.ExecuteResponse{Type: pb.OutputType_EXIT, ExitCode: -1})
		return nil
	}
	_ = outW.Close()
	_ = errW.Close()
	applyLimits(cmd.Process.Pid)

	// Kill the group only on timeout or output cap, never because the client
	// went away, so background children survive the RPC. Detached start
	// commands belong to the supervisor.
	if !req.Detach {
		killTimer := time.AfterFunc(timeout, func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		})
		defer killTimer.Stop()
	}

	var totalBytes atomic.Int64
	var truncated atomic.Bool
	var sendMu sync.Mutex // ServerStream.Send is not safe for concurrent use
	var wg sync.WaitGroup

	streamOutput := func(pipe *os.File, t pb.OutputType) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := pipe.Read(buf)
			if n > 0 {
				// The cap keeps a runaway command from filling envd's buffers;
				// a raw stream (tar egress) opts out per request.
				if totalBytes.Add(int64(n)) > maxOutput && !req.NoOutputCap {
					truncated.Store(true)
					if cmd.Process != nil {
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}
					return
				}
				sendMu.Lock()
				_ = stream.Send(&pb.ExecuteResponse{
					Type: t,
					Data: append([]byte(nil), buf[:n]...),
				})
				sendMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}

	wg.Add(2)
	go streamOutput(outR, pb.OutputType_STDOUT)
	go streamOutput(errR, pb.OutputType_STDERR)

	// Wait for the direct child, not pipe EOF; the short grace drains in-flight output.
	procState, _ := cmd.Process.Wait()
	if req.NoOutputCap {
		// A raw stream is byte-exact by contract: drain to pipe EOF instead of
		// racing the 50 ms grace against a 30 MB tail still in flight.
		wg.Wait()
	}
	time.Sleep(50 * time.Millisecond)
	_ = outR.Close()
	_ = errR.Close()
	wg.Wait()

	if truncated.Load() {
		_ = stream.Send(&pb.ExecuteResponse{Type: pb.OutputType_TRUNCATED})
	}

	exitCode := 0
	if procState != nil {
		exitCode = procState.ExitCode()
	}
	return stream.Send(&pb.ExecuteResponse{
		Type:     pb.OutputType_EXIT,
		ExitCode: int32(exitCode),
	})
}

// homeDir is the sandbox working directory; relative file paths resolve against
// it so files land where Execute runs commands. A var so tests can point it at a temp dir.
var homeDir = "/root"

// errInvalidPath is deliberately generic so a caller cannot tell an escape attempt from not-found.
var errInvalidPath = errors.New("invalid path")

// confineToWorkspace resolves p against homeDir and rejects anything that
// lands outside it, including via a symlink. Shell-backed ops that bypass
// envd (Mkdir, Move, Remove, Stat, List) have their own guard in confineShell.
func confineToWorkspace(p string) (string, error) {
	if p == "" {
		p = "."
	}
	// filepath.Join does not special-case an absolute second argument.
	var full string
	if filepath.IsAbs(p) {
		full = filepath.Clean(p)
	} else {
		full = filepath.Clean(filepath.Join(homeDir, p))
	}
	if !pathWithinRoot(homeDir, full) {
		return "", errInvalidPath
	}

	// Compare against a resolved root, or every path is rejected when homeDir
	// itself sits behind a symlink (macOS /var -> /private/var in tests).
	canonicalRoot := homeDir
	if resolved, err := filepath.EvalSymlinks(homeDir); err == nil {
		canonicalRoot = resolved
	}

	// EvalSymlinks needs an existing target, so walk up to the deepest existing
	// ancestor and re-append the tail; a new leaf then passes while a symlink
	// anywhere in the existing prefix is still caught.
	existing, rest := full, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			target := filepath.Join(resolved, rest)
			if !pathWithinRoot(canonicalRoot, target) {
				return "", errInvalidPath
			}
			break
		}
		if existing == homeDir || existing == string(filepath.Separator) || existing == "." {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = filepath.Dir(existing)
	}
	return full, nil
}

// pathWithinRoot reports whether target is root or lexically nested under it.
func pathWithinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// fileChunk is the ReadFile stream chunk size; WriteFile accepts any size the
// message cap allows.
const fileChunk = 1 << 20

// WriteFile streams a file in. The first chunk names the path; bytes go to a
// temp file beside the target and are renamed into place on EOF, so a stream
// cut mid-way leaves no partial file. Every upload lands 0644 — the request
// used to carry a mode nothing above this API could set.
func (s *envdServer) WriteFile(stream pb.EnvdService_WriteFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	const mode fs.FileMode = 0644
	path, err := confineToWorkspace(first.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".envd-upload-*")
	if err != nil {
		return err
	}
	var total int64
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	chunk := first
	for {
		n, err := tmp.Write(chunk.Data)
		total += int64(n)
		if err != nil {
			return fail(err)
		}
		if chunk, err = stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fail(err)
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return stream.SendAndClose(&pb.WriteFileResponse{Bytes: total})
}

// ReadFile streams a file out in fileChunk pieces.
func (s *envdServer) ReadFile(req *pb.ReadFileRequest, stream pb.EnvdService_ReadFileServer) error {
	path, err := confineToWorkspace(req.Path)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only
	for {
		// A fresh buffer per chunk, not one reused across sends: gRPC only
		// promises the message is untouched until Send returns, and an
		// interceptor or stats handler may read it after that. Execute's
		// streamOutput already copies for the same reason.
		buf := make([]byte, fileChunk)
		n, err := f.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.ReadFileResponse{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// SetEnv merges the request into /etc/template.env and rewrites the profile
// drop-in. Merge, not replace: a /config refresh must not erase the create env,
// and a fork re-sending the parent's env is idempotent.
func (s *envdServer) SetEnv(_ context.Context, req *pb.SetEnvRequest) (*pb.SetEnvResponse, error) {
	if err := mergeTemplateEnv(templateEnvDir, req.Env); err != nil {
		return nil, err
	}
	// This is where PD_DESKTOP=1 arrives on Firecracker: the create env is
	// pushed over the control channel after boot, not baked into envd's own
	// environment. Idempotent — the stack starts at most once.
	maybeStartDesktop()
	return &pb.SetEnvResponse{}, writeProfileDropIn(templateEnvDir, profileDir)
}

func (s *envdServer) ListFiles(_ context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	path, err := confineToWorkspace(req.Path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var result []*pb.FileEntry
	for _, e := range entries {
		info, _ := e.Info()
		fe := &pb.FileEntry{Name: e.Name(), IsDir: e.IsDir()}
		if info != nil {
			fe.Size = info.Size()
			fe.ModifiedUnix = info.ModTime().Unix()
		}
		result = append(result, fe)
	}
	return &pb.ListFilesResponse{Entries: result}, nil
}
