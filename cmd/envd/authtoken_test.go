package main

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

func TestAuthUnaryEnforces(t *testing.T) {
	tok, hash := sbtoken.Mint()
	ic := authUnary(newHashStore(hash))
	called := false
	handler := func(_ context.Context, _ any) (any, error) { called = true; return "ok", nil }
	exec := &grpc.UnaryServerInfo{FullMethod: "/envdpb.EnvdService/Execute"}
	health := &grpc.UnaryServerInfo{FullMethod: "/envdpb.EnvdService/Health"}

	if _, err := ic(context.Background(), nil, exec, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token: err=%v, want Unauthenticated", err)
	}
	if called {
		t.Fatal("handler ran without a token")
	}

	wrong := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sbtoken.MetadataKey, "deadbeef"))
	if _, err := ic(wrong, nil, exec, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token: err=%v, want Unauthenticated", err)
	}

	ok := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sbtoken.MetadataKey, tok))
	if _, err := ic(ok, nil, exec, handler); err != nil || !called {
		t.Fatalf("right token: err=%v called=%v", err, called)
	}

	called = false
	if _, err := ic(context.Background(), nil, health, handler); err != nil || !called {
		t.Fatalf("Health should be open: err=%v called=%v", err, called)
	}
}

func TestReadTokenHashFromEnv(t *testing.T) {
	t.Setenv("ENVD_TOKEN_HASH", "abc123")
	if got := readTokenHash(); got != "abc123" {
		t.Fatalf("readTokenHash() = %q, want abc123", got)
	}
}

// TestTokenAuthOptionsInstallsInterceptorsWithHash runs the options through a real gRPC server.
func TestTokenAuthOptionsInstallsInterceptorsWithHash(t *testing.T) {
	tok, hash := sbtoken.Mint()

	// ListFiles goes through confineToWorkspace, so homeDir must be listable.
	old := homeDir
	homeDir = t.TempDir()
	t.Cleanup(func() { homeDir = old })

	opts := tokenAuthOptions(newHashStore(hash))
	if len(opts) == 0 {
		t.Fatal("tokenAuthOptions(hash) installed no interceptors with a hash present")
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	srv := grpc.NewServer(opts...)
	pb.RegisterEnvdServiceServer(srv, &envdServer{})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := pb.NewEnvdServiceClient(conn)

	if _, err := client.Health(context.Background(), &pb.HealthRequest{}); err != nil {
		t.Fatalf("Health without a token: %v (want success — Health stays open)", err)
	}

	if _, err := client.ListFiles(context.Background(), &pb.ListFilesRequest{Path: "."}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ListFiles without a token: err=%v, want Unauthenticated", err)
	}

	authed := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(sbtoken.MetadataKey, tok))
	if _, err := client.ListFiles(authed, &pb.ListFilesRequest{Path: "."}); err != nil {
		t.Fatalf("ListFiles with a valid token: %v", err)
	}
}
