package envd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/envdproto"
	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

// tokenCreds attaches the per-sandbox access token to every envd call as gRPC metadata.
// The channel is plaintext on the VM network; the token is the auth boundary.
type tokenCreds struct{ token string }

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{sbtoken.MetadataKey: t.token}, nil
}
func (t tokenCreds) RequireTransportSecurity() bool { return false }

// maxMsgBytes is the envd message cap on both sides (see cmd/envd/authtoken.go).
const maxMsgBytes = 256 << 20

type Client struct {
	conn   *grpc.ClientConn
	client pb.EnvdServiceClient
	// protocol is the guest's envdproto level, read from Health at connect.
	protocol int
}

// Protocol is the guest envd's capability level (see internal/envdproto). Gate a
// feature newer than level 1 on it before calling the RPC that needs it.
func (c *Client) Protocol() int { return c.protocol }

// ErrEnvdUnauthenticated means envd is Health-ready but refuses this sandbox's token.
var ErrEnvdUnauthenticated = errors.New("envd refused this sandbox's access token")

// Connect waits until envd reports Ready without checking credentials.
// Only for the golden template VM, which boots with no token; real sandboxes
// must use ConnectWithToken.
func Connect(vsockPath string, timeout time.Duration) (*Client, error) {
	return connect(vsockPath, "", false, timeout)
}

// ConnectWithToken waits until envd is Ready and accepts token. An empty token
// fails rather than meaning "auth off".
func ConnectWithToken(vsockPath, token string, timeout time.Duration) (*Client, error) {
	return connect(vsockPath, token, true, timeout)
}

func connect(vsockPath, token string, requireAuth bool, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)

	dialOpts := []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialVsock(vsockPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// gRPC's 4 MiB default silently capped every file read; the node's own
		// request cap is the real bound.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgBytes)),
	}
	if token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(tokenCreds{token: token}))
	}

	// NewClient connects lazily; the Health loop below absorbs the connect wait.
	conn, err := grpc.NewClient("passthrough:///"+vsockPath, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("envd dial failed after %v: %w", timeout, err)
	}

	c := pb.NewEnvdServiceClient(conn)

	// authErr lets the timeout error distinguish "never came up" from "refused us".
	var authErr error
	for time.Now().Before(deadline) {
		resp, herr := c.Health(context.Background(), &pb.HealthRequest{})
		if herr == nil && resp.Ready {
			ready := &Client{conn: conn, client: c, protocol: envdproto.Parse(resp.Version)}
			if !requireAuth {
				return ready, nil
			}
			// Health is exempt from auth, so probe the credential with the
			// cheapest authenticated method.
			_, aerr := c.ListFiles(context.Background(), &pb.ListFilesRequest{Path: "."})
			if aerr == nil {
				return ready, nil
			}
			authErr = aerr
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = conn.Close() //nolint:errcheck

	if authErr != nil {
		if status.Code(authErr) == codes.Unauthenticated {
			return nil, fmt.Errorf(
				"envd at %s is running and Health-ready but refused the access token for %v "+
					"(the guest holds no matching hash, or a different one): %w",
				vsockPath, timeout, ErrEnvdUnauthenticated)
		}
		return nil, fmt.Errorf("envd at %s is Health-ready but the authenticated readiness probe failed for %v: %w",
			vsockPath, timeout, authErr)
	}
	return nil, fmt.Errorf("envd not ready after %v", timeout)
}

type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Truncated bool
}

func (c *Client) Execute(ctx context.Context, cmd, cwd string, timeout time.Duration) (*ExecResult, error) {
	return c.Run(ctx, ExecOpts{Cmd: cmd, Cwd: cwd, Timeout: timeout})
}

// ExecOpts is one exec. Stdin is delivered whole; NoOutputCap lifts envd's
// 10 MiB output kill for a raw stream.
type ExecOpts struct {
	Cmd, Cwd    string
	Timeout     time.Duration
	Stdin       []byte
	NoOutputCap bool
	Detach      bool
}

func (o ExecOpts) req() *pb.ExecuteRequest {
	return &pb.ExecuteRequest{
		Cmd: o.Cmd, Cwd: o.Cwd, TimeoutSeconds: int32(o.Timeout.Seconds()),
		Stdin: o.Stdin, NoOutputCap: o.NoOutputCap, Detach: o.Detach,
	}
}

// Run executes o and collects the output.
func (c *Client) Run(ctx context.Context, o ExecOpts) (*ExecResult, error) {
	return c.execute(ctx, o.req())
}

// RunStream executes o and calls onFrame for each output message.
func (c *Client) RunStream(ctx context.Context, o ExecOpts, onFrame func(StreamFrame)) error {
	return c.executeStream(ctx, o.req(), onFrame)
}

// ExecuteDetached runs a start command whose process group outlives the RPC.
func (c *Client) ExecuteDetached(ctx context.Context, cmd, cwd string, timeout time.Duration) (*ExecResult, error) {
	return c.execute(ctx, &pb.ExecuteRequest{
		Cmd:            cmd,
		Cwd:            cwd,
		TimeoutSeconds: int32(timeout.Seconds()),
		Detach:         true,
	})
}

func (c *Client) execute(ctx context.Context, req *pb.ExecuteRequest) (*ExecResult, error) {
	stream, err := c.client.Execute(ctx, req)
	if err != nil {
		return nil, err
	}

	result := &ExecResult{}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, err
		}
		switch msg.Type {
		case pb.OutputType_STDOUT:
			result.Stdout = append(result.Stdout, msg.Data...)
		case pb.OutputType_STDERR:
			result.Stderr = append(result.Stderr, msg.Data...)
		case pb.OutputType_EXIT:
			result.ExitCode = int(msg.ExitCode)
		case pb.OutputType_TRUNCATED:
			result.Truncated = true
		}
	}
	return result, nil
}

// StreamFrame is one incremental exec output message.
type StreamFrame struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Exited    bool
	Truncated bool
}

// ExecuteStream runs a command and calls onFrame for each output message.
// onFrame runs on the receive loop and must not block for long.
func (c *Client) ExecuteStream(ctx context.Context, cmd, cwd string, timeout time.Duration, onFrame func(StreamFrame)) error {
	return c.RunStream(ctx, ExecOpts{Cmd: cmd, Cwd: cwd, Timeout: timeout}, onFrame)
}

func (c *Client) executeStream(ctx context.Context, req *pb.ExecuteRequest, onFrame func(StreamFrame)) error {
	stream, err := c.client.Execute(ctx, req)
	if err != nil {
		return err
	}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch msg.Type {
		case pb.OutputType_STDOUT:
			onFrame(StreamFrame{Stdout: msg.Data})
		case pb.OutputType_STDERR:
			onFrame(StreamFrame{Stderr: msg.Data})
		case pb.OutputType_EXIT:
			onFrame(StreamFrame{ExitCode: int(msg.ExitCode), Exited: true})
		case pb.OutputType_TRUNCATED:
			onFrame(StreamFrame{Truncated: true})
		}
	}
}

// fileChunk is the upload chunk size; envd streams reads at the same size.
const fileChunk = 1 << 20

func (c *Client) WriteFile(ctx context.Context, path string, data []byte) error {
	_, err := c.WriteFrom(ctx, path, bytes.NewReader(data))
	return err
}

// WriteFrom streams r into path in fileChunk pieces and returns envd's byte
// count. Nothing is buffered beyond one chunk.
func (c *Client) WriteFrom(ctx context.Context, path string, r io.Reader) (int64, error) {
	stream, err := c.client.WriteFile(ctx)
	if err != nil {
		return 0, err
	}
	first := true
	for {
		// Not one buffer reused across Sends: gRPC only promises the message
		// is untouched until Send returns, so a still-queued chunk could be
		// overwritten by the next read.
		buf := make([]byte, fileChunk)
		n, rerr := r.Read(buf)
		if n > 0 || first {
			msg := &pb.WriteFileRequest{Data: buf[:n]}
			if first {
				msg.Path = path
				first = false
			}
			if serr := stream.Send(msg); serr != nil {
				// The server's error surfaces on CloseAndRecv, not Send.
				if errors.Is(serr, io.EOF) {
					break
				}
				return 0, serr
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return 0, rerr
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return 0, err
	}
	return resp.Bytes, nil
}

func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.ReadTo(ctx, path, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadTo streams path into w.
func (c *Client) ReadTo(ctx context.Context, path string, w io.Writer) error {
	stream, err := c.client.ReadFile(ctx, &pb.ReadFileRequest{Path: path})
	if err != nil {
		return err
	}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := w.Write(msg.Data); err != nil {
			return err
		}
	}
}

// SetEnv merges env into the guest's /etc/template.env so it reaches every
// session kind, not only the exec command line.
func (c *Client) SetEnv(ctx context.Context, env map[string]string) error {
	_, err := c.client.SetEnv(ctx, &pb.SetEnvRequest{Env: env})
	return err
}

// Close tears down the gRPC connection.
func (c *Client) Close() { _ = c.conn.Close() } //nolint:errcheck
