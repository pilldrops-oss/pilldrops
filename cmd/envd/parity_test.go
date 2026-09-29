package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

// SetEnv must reach the session env and the login-shell drop-in, and be
// token-gated like every RPC but Health.
func TestSetEnvReachesSessionAndProfile(t *testing.T) {
	templateEnvDir, profileDir = t.TempDir(), t.TempDir()
	defer func() { templateEnvDir, profileDir = "/etc", "/etc/profile.d" }()

	srv := &envdServer{}
	if _, err := srv.SetEnv(context.Background(), &pb.SetEnvRequest{Env: map[string]string{"A": "1", "SECRET": "it's"}}); err != nil {
		t.Fatal(err)
	}
	if env := buildSessionEnv(nil); !slices.Contains(env, "A=1") || !slices.Contains(env, "SECRET=it's") {
		t.Errorf("session env lacks the SetEnv vars:\n%s", strings.Join(env, "\n"))
	}
	drop, err := os.ReadFile(filepath.Join(profileDir, "zz-pilldrops-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(drop), "export A='1'") || !strings.Contains(string(drop), `export SECRET='it'\''s'`) {
		t.Errorf("profile drop-in lacks the export:\n%s", drop)
	}
	// Merge, not replace: a second call keeps A.
	if _, err := srv.SetEnv(context.Background(), &pb.SetEnvRequest{Env: map[string]string{"B": "2"}}); err != nil {
		t.Fatal(err)
	}
	if env := buildSessionEnv(nil); !slices.Contains(env, "A=1") || !slices.Contains(env, "B=2") {
		t.Errorf("second SetEnv erased the first:\n%s", strings.Join(env, "\n"))
	}
	if st, _ := os.Stat(filepath.Join(templateEnvDir, "template.env")); st.Mode().Perm() != 0600 {
		t.Errorf("template.env mode = %o, want 0600", st.Mode().Perm())
	}
	// Auth: no token → refused; Health stays open.
	if authorized("/envd.EnvdService/SetEnv", metadata.NewIncomingContext(context.Background(), nil), "somehash") {
		t.Error("SetEnv accepted a call with no token")
	}
	if !authorized("/envd.EnvdService/Health", context.Background(), "somehash") {
		t.Error("Health is not open")
	}
}

type fakeWriteStream struct {
	grpc.ServerStream
	chunks []*pb.WriteFileRequest
	cutErr error // returned once the chunks run out instead of io.EOF
	resp   *pb.WriteFileResponse
}

func (f *fakeWriteStream) Context() context.Context { return context.Background() }
func (f *fakeWriteStream) Recv() (*pb.WriteFileRequest, error) {
	if len(f.chunks) == 0 {
		if f.cutErr != nil {
			return nil, f.cutErr
		}
		return nil, io.EOF
	}
	c := f.chunks[0]
	f.chunks = f.chunks[1:]
	return c, nil
}
func (f *fakeWriteStream) SendAndClose(r *pb.WriteFileResponse) error { f.resp = r; return nil }

type fakeReadStream struct {
	grpc.ServerStream
	buf bytes.Buffer
}

func (f *fakeReadStream) Context() context.Context          { return context.Background() }
func (f *fakeReadStream) Send(r *pb.ReadFileResponse) error { f.buf.Write(r.Data); return nil }

func TestWriteFileStreamsAndLeavesNoPartialFile(t *testing.T) {
	homeDir = t.TempDir()
	defer func() { homeDir = "/root" }()
	srv := &envdServer{}

	data := make([]byte, 5<<20)
	_, _ = rand.Read(data)
	var chunks []*pb.WriteFileRequest
	for off := 0; off < len(data); off += 1 << 20 {
		c := &pb.WriteFileRequest{Data: data[off : off+1<<20]}
		if off == 0 {
			c.Path = "sub/big.bin"
		}
		chunks = append(chunks, c)
	}
	ws := &fakeWriteStream{chunks: chunks}
	if err := srv.WriteFile(ws); err != nil {
		t.Fatal(err)
	}
	if ws.resp == nil || ws.resp.Bytes != int64(len(data)) {
		t.Fatalf("response = %v, want bytes=%d", ws.resp, len(data))
	}
	if st, err := os.Stat(filepath.Join(homeDir, "sub/big.bin")); err != nil || st.Mode().Perm() != 0644 {
		t.Fatalf("stat: %v mode %v", err, st)
	}
	rs := &fakeReadStream{}
	if err := srv.ReadFile(&pb.ReadFileRequest{Path: "sub/big.bin"}, rs); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rs.buf.Bytes(), data) {
		t.Fatalf("round trip differs: got %d bytes", rs.buf.Len())
	}

	// Cut after chunk 2: no file at the path and no temp file left behind.
	cut := &fakeWriteStream{chunks: chunks[:2], cutErr: errors.New("stream reset")}
	cut.chunks[0].Path = "cut.bin"
	if err := srv.WriteFile(cut); err == nil {
		t.Fatal("a cut stream reported success")
	}
	if _, err := os.Stat(filepath.Join(homeDir, "cut.bin")); err == nil {
		t.Error("a partial file was left at the target path")
	}
	left, _ := filepath.Glob(filepath.Join(homeDir, ".envd-upload-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestExecStdinAndRawOutput(t *testing.T) {
	// stdin reaches the command; without it, cat sees EOF at once.
	stream := &fakeExecStream{ctx: context.Background()}
	if err := (&envdServer{}).Execute(&pb.ExecuteRequest{Cmd: "cat", Cwd: t.TempDir(), Stdin: []byte("abc"), TimeoutSeconds: 5}, stream); err != nil {
		t.Fatal(err)
	}
	if got := stream.combined(); got != "abc" {
		t.Errorf("stdin round trip = %q", got)
	}
	ctrl, took := runExecute(t, "cat", 5)
	if ctrl.combined() != "" || ctrl.exitCode(t) != 0 || took.Seconds() > 2 {
		t.Errorf("cat without stdin: out=%q exit=%d took=%v", ctrl.combined(), ctrl.exitCode(t), took)
	}

	// Over the cap: default stream is killed with TRUNCATED; raw carries it all.
	n := maxOutput + 1<<20
	def, _ := runExecute(t, "head -c "+strconv.Itoa(n)+" /dev/zero", 20)
	if !hasTruncated(def) {
		t.Error("default stream over the cap did not end with TRUNCATED")
	}
	raw := &fakeExecStream{ctx: context.Background()}
	if err := (&envdServer{}).Execute(&pb.ExecuteRequest{Cmd: "head -c " + strconv.Itoa(n) + " /dev/zero", Cwd: t.TempDir(), NoOutputCap: true, TimeoutSeconds: 20}, raw); err != nil {
		t.Fatal(err)
	}
	if hasTruncated(raw) || len(raw.combined()) != n {
		t.Errorf("raw stream: truncated=%v bytes=%d want %d", hasTruncated(raw), len(raw.combined()), n)
	}
}

func hasTruncated(s *fakeExecStream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.out {
		if r.Type == pb.OutputType_TRUNCATED {
			return true
		}
	}
	return false
}
