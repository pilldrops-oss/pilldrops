package main

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

type fakeExecStream struct {
	grpc.ServerStream
	ctx context.Context

	mu  sync.Mutex
	out []*pb.ExecuteResponse
}

func (f *fakeExecStream) Context() context.Context { return f.ctx }

func (f *fakeExecStream) Send(r *pb.ExecuteResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, r)
	return nil
}

func (f *fakeExecStream) combined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b []byte
	for _, r := range f.out {
		b = append(b, r.Data...)
	}
	return string(b)
}

func (f *fakeExecStream) exitCode(t *testing.T) int32 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.out {
		if r.Type == pb.OutputType_EXIT {
			return r.ExitCode
		}
	}
	t.Fatal("no EXIT frame in stream")
	return -1
}

func runExecute(t *testing.T, cmd string, timeoutSeconds int32) (*fakeExecStream, time.Duration) {
	t.Helper()
	stream := &fakeExecStream{ctx: context.Background()}
	srv := &envdServer{}
	t0 := time.Now()
	// The default cwd /root only exists in the guest.
	req := &pb.ExecuteRequest{Cmd: cmd, TimeoutSeconds: timeoutSeconds, Cwd: t.TempDir()}
	if err := srv.Execute(req, stream); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	return stream, time.Since(t0)
}

func processAlive(pattern string) bool {
	return exec.Command("pgrep", "-f", pattern).Run() == nil
}

func TestBackgroundChildSurvivesExecReturn(t *testing.T) {
	const marker = "63549" // unique sleep duration to pgrep for
	t.Cleanup(func() { exec.Command("pkill", "-f", "sleep "+marker).Run() })

	stream, dur := runExecute(t, "nohup sleep "+marker+" >/dev/null 2>&1 & echo started", 30)

	if got := stream.exitCode(t); got != 0 {
		t.Fatalf("exit code = %d, want 0; output: %q", got, stream.combined())
	}
	if dur > 5*time.Second {
		t.Fatalf("handler blocked %v; should return right after the shell exits", dur)
	}
	time.Sleep(300 * time.Millisecond) // let a buggy kill goroutine fire
	if !processAlive("sleep " + marker) {
		t.Fatal("background child was killed at RPC end (D-13 regression)")
	}
}

// A backgrounded compound runs in a subshell that inherits envd's stdout, so
// streaming must end on child exit rather than pipe EOF.
func TestBackgroundCompoundDoesNotBlockOnInheritedPipe(t *testing.T) {
	const marker = "63553"
	t.Cleanup(func() { exec.Command("pkill", "-f", "sleep "+marker).Run() })

	dir := t.TempDir()
	cmdStr := "cd " + dir + " && (sleep " + marker + " >/dev/null 2>&1) & echo backgrounded"
	stream, dur := runExecute(t, cmdStr, 30)

	if dur > 5*time.Second {
		t.Fatalf("handler blocked %v on an inherited pipe (D-13 subshell variant)", dur)
	}
	if out := stream.combined(); !strings.Contains(out, "backgrounded") {
		t.Fatalf("foreground output lost: %q", out)
	}
	if got := stream.exitCode(t); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
	time.Sleep(300 * time.Millisecond)
	if !processAlive("sleep " + marker) {
		t.Fatal("backgrounded compound child was killed (D-13 subshell variant)")
	}
}

func TestForegroundOutputAndExitCode(t *testing.T) {
	stream, _ := runExecute(t, "echo out-line; echo err-line 1>&2; exit 7", 30)
	out := stream.combined()
	if !strings.Contains(out, "out-line") || !strings.Contains(out, "err-line") {
		t.Fatalf("lost foreground output: %q", out)
	}
	if got := stream.exitCode(t); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
}

func TestForegroundDeadlineStillKills(t *testing.T) {
	const marker = "63551"
	t.Cleanup(func() { exec.Command("pkill", "-f", "sleep "+marker).Run() })

	stream, dur := runExecute(t, "sleep "+marker, 2)

	if dur > 6*time.Second {
		t.Fatalf("deadline did not fire: handler took %v for a 2s timeout", dur)
	}
	if dur < 1500*time.Millisecond {
		// A failed Start returns instantly with exit -1, which would pass the checks below.
		t.Fatalf("handler returned in %v — command never actually ran", dur)
	}
	if got := stream.exitCode(t); got == 0 {
		t.Fatal("killed-at-deadline command reported exit 0")
	}
	if processAlive("sleep " + marker) {
		t.Fatal("foreground command survived its deadline")
	}
}
