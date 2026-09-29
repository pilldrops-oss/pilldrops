package envd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

// fakeStream replays fixed ExecuteResponses, optionally ending in an error instead of EOF.
type fakeStream struct {
	grpc.ServerStreamingClient[pb.ExecuteResponse]
	msgs []*pb.ExecuteResponse
	i    int
	err  error
}

func (f *fakeStream) Recv() (*pb.ExecuteResponse, error) {
	if f.i < len(f.msgs) {
		m := f.msgs[f.i]
		f.i++
		return m, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return nil, io.EOF
}

type fakeEnvd struct {
	pb.EnvdServiceClient
	execReq  *pb.ExecuteRequest
	stream   *fakeStream
	execErr  error
	writeReq *pb.WriteFileRequest
	readResp *pb.ReadFileResponse
	readErr  error
}

func (f *fakeEnvd) Execute(_ context.Context, in *pb.ExecuteRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.ExecuteResponse], error) {
	f.execReq = in
	if f.execErr != nil {
		return nil, f.execErr
	}
	if f.stream == nil {
		f.stream = &fakeStream{}
	}
	return f.stream, nil
}

// fakeWriteClient collects the streamed chunks into one request for assertions.
type fakeWriteClient struct {
	grpc.ClientStreamingClient[pb.WriteFileRequest, pb.WriteFileResponse]
	f *fakeEnvd
}

func (w *fakeWriteClient) Send(m *pb.WriteFileRequest) error {
	if w.f.writeReq == nil {
		w.f.writeReq = &pb.WriteFileRequest{Path: m.Path}
	}
	w.f.writeReq.Data = append(w.f.writeReq.Data, m.Data...)
	return nil
}
func (w *fakeWriteClient) CloseAndRecv() (*pb.WriteFileResponse, error) {
	return &pb.WriteFileResponse{Bytes: int64(len(w.f.writeReq.Data))}, nil
}

func (f *fakeEnvd) WriteFile(_ context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[pb.WriteFileRequest, pb.WriteFileResponse], error) {
	return &fakeWriteClient{f: f}, nil
}

type fakeReadClient struct {
	grpc.ServerStreamingClient[pb.ReadFileResponse]
	msgs []*pb.ReadFileResponse
}

func (r *fakeReadClient) Recv() (*pb.ReadFileResponse, error) {
	if len(r.msgs) == 0 {
		return nil, io.EOF
	}
	m := r.msgs[0]
	r.msgs = r.msgs[1:]
	return m, nil
}

func (f *fakeEnvd) ReadFile(_ context.Context, _ *pb.ReadFileRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.ReadFileResponse], error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return &fakeReadClient{msgs: []*pb.ReadFileResponse{f.readResp}}, nil
}

func out(t pb.OutputType, data string) *pb.ExecuteResponse {
	return &pb.ExecuteResponse{Type: t, Data: []byte(data)}
}

func TestTokenIsAttachedToEveryCall(t *testing.T) {
	creds := tokenCreds{token: "sbx_secret"}
	md, err := creds.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if md[sbtoken.MetadataKey] != "sbx_secret" {
		t.Errorf("metadata[%s] = %q, want the token", sbtoken.MetadataKey, md[sbtoken.MetadataKey])
	}
	// gRPC refuses per-RPC credentials over an insecure channel if this is true.
	if creds.RequireTransportSecurity() {
		t.Error("RequireTransportSecurity must be false: envd's channel is plaintext on the VM network")
	}
	if len(md) != 1 {
		t.Errorf("metadata carries %d entries, want just the token: %v", len(md), md)
	}
	// gRPC lowercases header keys, so a mixed-case key silently never matches.
	if sbtoken.MetadataKey != strings.ToLower(sbtoken.MetadataKey) {
		t.Errorf("metadata key %q is not lowercase; gRPC will not match it", sbtoken.MetadataKey)
	}
}

func TestExecuteAccumulatesEachStreamKind(t *testing.T) {
	f := &fakeEnvd{stream: &fakeStream{msgs: []*pb.ExecuteResponse{
		out(pb.OutputType_STDOUT, "hello "),
		out(pb.OutputType_STDERR, "warn"),
		out(pb.OutputType_STDOUT, "world"),
		{Type: pb.OutputType_EXIT, ExitCode: 3},
	}}}
	c := &Client{client: f}

	res, err := c.Execute(context.Background(), "echo", "/work", 5*time.Second)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(res.Stdout) != "hello world" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello world")
	}
	if string(res.Stderr) != "warn" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "warn")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if res.Truncated {
		t.Error("truncated set with no TRUNCATED frame")
	}
	if bytes.Contains(res.Stdout, []byte("warn")) {
		t.Error("stderr leaked into stdout")
	}
}

func TestExecuteReportsTruncation(t *testing.T) {
	f := &fakeEnvd{stream: &fakeStream{msgs: []*pb.ExecuteResponse{
		out(pb.OutputType_STDOUT, "a lot of output"),
		{Type: pb.OutputType_TRUNCATED},
		{Type: pb.OutputType_EXIT, ExitCode: 0},
	}}}
	c := &Client{client: f}

	res, err := c.Execute(context.Background(), "cat big", "", time.Second)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Truncated {
		t.Error("truncated flag lost; the caller cannot tell a short answer from a cut-off one")
	}
}

func TestExecuteReturnsPartialOutputOnStreamError(t *testing.T) {
	boom := errors.New("connection reset")
	f := &fakeEnvd{stream: &fakeStream{
		msgs: []*pb.ExecuteResponse{out(pb.OutputType_STDOUT, "got this far")},
		err:  boom,
	}}
	c := &Client{client: f}

	res, err := c.Execute(context.Background(), "sleep 10", "", time.Second)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the stream error", err)
	}
	if res == nil || string(res.Stdout) != "got this far" {
		t.Errorf("partial output lost: %+v", res)
	}
}

func TestExecuteReturnsNilResultWhenTheCallItselfFails(t *testing.T) {
	f := &fakeEnvd{execErr: errors.New("unavailable")}
	c := &Client{client: f}

	res, err := c.Execute(context.Background(), "x", "", time.Second)
	if err == nil {
		t.Fatal("want an error when Execute cannot be issued")
	}
	if res != nil {
		t.Errorf("res = %+v, want nil when no stream was ever opened", res)
	}
}

func TestExecuteDetachedSetsDetach(t *testing.T) {
	f := &fakeEnvd{}
	c := &Client{client: f}

	if _, err := c.ExecuteDetached(context.Background(), "npm start", "/app", 30*time.Second); err != nil {
		t.Fatalf("ExecuteDetached: %v", err)
	}
	if !f.execReq.Detach {
		t.Error("Detach not set; the started server would be reaped with its RPC")
	}

	f2 := &fakeEnvd{}
	c2 := &Client{client: f2}
	if _, err := c2.Execute(context.Background(), "ls", "/app", time.Second); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f2.execReq.Detach {
		t.Error("Detach set on a plain Execute; every command would outlive its call")
	}
}

func TestExecuteForwardsCommandAndCwd(t *testing.T) {
	f := &fakeEnvd{}
	c := &Client{client: f}
	if _, err := c.Execute(context.Background(), "make test", "/srv/app", 90*time.Second); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.execReq.Cmd != "make test" || f.execReq.Cwd != "/srv/app" {
		t.Errorf("cmd=%q cwd=%q, want them forwarded verbatim", f.execReq.Cmd, f.execReq.Cwd)
	}
	if f.execReq.TimeoutSeconds != 90 {
		t.Errorf("timeout = %d, want 90", f.execReq.TimeoutSeconds)
	}
}

// The wire field is whole seconds, and envd reads 0 as its 30s default.
// Pinned, not fixed: no caller passes sub-second timeouts.
func TestSubSecondTimeoutTruncatesToZero(t *testing.T) {
	f := &fakeEnvd{}
	c := &Client{client: f}
	if _, err := c.Execute(context.Background(), "x", "", 500*time.Millisecond); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.execReq.TimeoutSeconds != 0 {
		t.Errorf("TimeoutSeconds = %d; this test documents the truncation, update it "+
			"deliberately if the field gains sub-second resolution", f.execReq.TimeoutSeconds)
	}
}

func TestExecuteStreamRelaysFramesInOrder(t *testing.T) {
	f := &fakeEnvd{stream: &fakeStream{msgs: []*pb.ExecuteResponse{
		out(pb.OutputType_STDOUT, "one"),
		out(pb.OutputType_STDERR, "two"),
		{Type: pb.OutputType_TRUNCATED},
		{Type: pb.OutputType_EXIT, ExitCode: 7},
	}}}
	c := &Client{client: f}

	var got []StreamFrame
	err := c.ExecuteStream(context.Background(), "run", "", time.Second, func(fr StreamFrame) {
		got = append(got, fr)
	})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d frames, want 4: %+v", len(got), got)
	}
	if string(got[0].Stdout) != "one" {
		t.Errorf("frame 0 = %+v, want stdout 'one'", got[0])
	}
	if string(got[1].Stderr) != "two" {
		t.Errorf("frame 1 = %+v, want stderr 'two'", got[1])
	}
	if !got[2].Truncated {
		t.Errorf("frame 2 = %+v, want truncated", got[2])
	}
	if !got[3].Exited || got[3].ExitCode != 7 {
		t.Errorf("frame 3 = %+v, want exited with 7", got[3])
	}
}

func TestExecuteStreamStopsOnError(t *testing.T) {
	boom := errors.New("stream broke")
	f := &fakeEnvd{stream: &fakeStream{
		msgs: []*pb.ExecuteResponse{out(pb.OutputType_STDOUT, "partial")},
		err:  boom,
	}}
	c := &Client{client: f}

	n := 0
	err := c.ExecuteStream(context.Background(), "run", "", time.Second, func(StreamFrame) { n++ })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the stream error", err)
	}
	if n != 1 {
		t.Errorf("delivered %d frames, want the 1 that arrived before the failure", n)
	}
}

func TestExecuteStreamEndsCleanlyOnEOF(t *testing.T) {
	f := &fakeEnvd{stream: &fakeStream{msgs: []*pb.ExecuteResponse{out(pb.OutputType_STDOUT, "x")}}}
	c := &Client{client: f}
	if err := c.ExecuteStream(context.Background(), "run", "", time.Second, func(StreamFrame) {}); err != nil {
		t.Errorf("EOF reported as an error: %v", err)
	}
}

func TestWriteFileForwardsPathAndBytes(t *testing.T) {
	f := &fakeEnvd{}
	c := &Client{client: f}
	payload := []byte{0x00, 0xff, 'h', 'i', '\n'}
	if err := c.WriteFile(context.Background(), "/work/a.bin", payload); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if f.writeReq.Path != "/work/a.bin" {
		t.Errorf("path = %q", f.writeReq.Path)
	}
	if !bytes.Equal(f.writeReq.Data, payload) {
		t.Errorf("data = %v, want %v", f.writeReq.Data, payload)
	}
}

func TestReadFile(t *testing.T) {
	want := []byte("file contents")
	c := &Client{client: &fakeEnvd{readResp: &pb.ReadFileResponse{Data: want}}}
	got, err := c.ReadFile(context.Background(), "/work/a.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	cErr := &Client{client: &fakeEnvd{readErr: errors.New("no such file")}}
	got, err = cErr.ReadFile(context.Background(), "/nope")
	if err == nil {
		t.Fatal("want an error")
	}
	if got != nil {
		t.Errorf("got %v alongside an error, want nil", got)
	}
}
