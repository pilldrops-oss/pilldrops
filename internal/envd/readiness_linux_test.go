//go:build linux

package envd

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

// These run over a real gRPC server on TCP because the property under test is
// the interaction between the health loop, the interceptor and the metadata.
// Linux-only because the !linux dialVsock ignores its address and dials localhost:6001.

// fakeEnvdServer mirrors cmd/envd: Health is open, every other method needs a
// token matching the current stored hash, read atomically at call time.
type fakeEnvdServer struct {
	pb.UnimplementedEnvdServiceServer
	hash      atomic.Value // string
	listErr   atomic.Value // error, fails ListFiles for a non-auth reason
	listCalls atomic.Int32
}

func (s *fakeEnvdServer) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Ready: true, Version: "test"}, nil
}

func (s *fakeEnvdServer) ListFiles(context.Context, *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	s.listCalls.Add(1)
	if e, _ := s.listErr.Load().(error); e != nil {
		return nil, e
	}
	return &pb.ListFilesResponse{}, nil
}

func (s *fakeEnvdServer) set(hash string) { s.hash.Store(hash) }

func (s *fakeEnvdServer) authorize(fullMethod string, ctx context.Context) bool {
	if strings.HasSuffix(fullMethod, "/Health") {
		return true
	}
	want, _ := s.hash.Load().(string)
	var tok string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(sbtoken.MetadataKey); len(v) > 0 {
			tok = v[0]
		}
	}
	return sbtoken.Verify(tok, want)
}

// startFakeEnvd serves on 127.0.0.1 and returns its "ip:port" address.
func startFakeEnvd(t *testing.T) (*fakeEnvdServer, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeEnvdServer{}
	s.set("")
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if !s.authorize(info.FullMethod, ctx) {
				return nil, status.Error(codes.Unauthenticated, "missing or invalid "+sbtoken.MetadataKey)
			}
			return h(ctx, req)
		}),
	)
	pb.RegisterEnvdServiceServer(srv, s)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	return s, l.Addr().String()
}

// Both halves must be one test: "allowed after" alone also passes against a
// readiness check that never looks at the credential.
func TestReadinessIsRefusedBeforeTheCredentialAndAllowedAfter(t *testing.T) {
	s, addr := startFakeEnvd(t)
	token, hash := sbtoken.Mint()

	// No credential yet, the state a golden-snapshot restore leaves the guest in.
	c, err := ConnectWithToken(addr, token, 700*time.Millisecond)
	if err == nil {
		c.Close()
		t.Fatal("readiness passed against a guest with NO credential; a create would " +
			"have reported this sandbox ready and every exec afterwards would answer Unauthenticated")
	}
	if !errors.Is(err, ErrEnvdUnauthenticated) {
		t.Fatalf("err = %v, want ErrEnvdUnauthenticated", err)
	}

	s.set(hash)
	c, err = ConnectWithToken(addr, token, 5*time.Second)
	if err != nil {
		t.Fatalf("readiness still refused after the credential was set: %v", err)
	}
	c.Close()
}

func TestReadinessRefusesTheWrongToken(t *testing.T) {
	s, addr := startFakeEnvd(t)
	_, hash := sbtoken.Mint()
	s.set(hash)
	other, _ := sbtoken.Mint()

	c, err := ConnectWithToken(addr, other, 700*time.Millisecond)
	if err == nil {
		c.Close()
		t.Fatal("a different sandbox's valid token passed readiness")
	}
	if !errors.Is(err, ErrEnvdUnauthenticated) {
		t.Fatalf("err = %v, want ErrEnvdUnauthenticated", err)
	}
}

// An empty token (ENVD_AUTH=0) is a failure, not "auth off": envd binds before
// the credential arrives, so create would succeed and every exec would fail.
func TestReadinessRefusesAnEmptyToken(t *testing.T) {
	_, addr := startFakeEnvd(t)

	c, err := ConnectWithToken(addr, "", 700*time.Millisecond)
	if err == nil {
		c.Close()
		t.Fatal("ConnectWithToken accepted an empty token; a node with envd auth off would " +
			"create sandboxes that answer Unauthenticated to every command")
	}
	if !errors.Is(err, ErrEnvdUnauthenticated) {
		t.Fatalf("err = %v, want ErrEnvdUnauthenticated", err)
	}
}

// The golden template VM boots with no token, so Connect must never probe auth.
func TestConnectStaysHealthOnlyForTheGoldenVM(t *testing.T) {
	s, addr := startFakeEnvd(t)

	c, err := Connect(addr, 5*time.Second)
	if err != nil {
		t.Fatalf("Connect: %v — the golden VM has no credential by design and must still become ready", err)
	}
	c.Close()
	if n := s.listCalls.Load(); n != 0 {
		t.Errorf("Connect made %d authenticated call(s); it must check Health only", n)
	}
}

func TestReadinessDistinguishesRefusalFromOtherFaults(t *testing.T) {
	s, addr := startFakeEnvd(t)
	token, hash := sbtoken.Mint()
	s.set(hash)
	s.listErr.Store(status.Error(codes.Internal, "disk on fire"))

	_, err := ConnectWithToken(addr, token, 700*time.Millisecond)
	if err == nil {
		t.Fatal("want an error when the authenticated probe fails")
	}
	if errors.Is(err, ErrEnvdUnauthenticated) {
		t.Fatalf("an Internal error was reported as a credential refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error %q drops the underlying reason", err)
	}
}

func TestReadinessReportsAnUnreachableEnvdAsNotReady(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	_, err = ConnectWithToken(addr, "sometoken", 300*time.Millisecond)
	if err == nil {
		t.Fatal("want an error against a dead address")
	}
	if errors.Is(err, ErrEnvdUnauthenticated) {
		t.Fatalf("an unreachable envd was reported as a credential refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error %q should say envd never became ready", err)
	}
}
