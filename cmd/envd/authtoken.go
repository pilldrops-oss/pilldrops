package main

// Per-sandbox access-token enforcement for envd's gRPC. The hash is read from
// the hashStore at call time because a snapshot-restored guest receives its
// credential after boot over vsock. An empty hash refuses every authenticated
// method; there is no "auth disabled" mode. Health is always open.

import (
	"context"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

var errUnauthenticated = status.Error(codes.Unauthenticated, "missing or invalid "+sbtoken.MetadataKey)

// readTokenHash returns this sandbox's token hash from ENVD_TOKEN_HASH (docker)
// or the envd.token_hash= kernel cmdline arg (firecracker), or "".
func readTokenHash() string {
	if h := strings.TrimSpace(os.Getenv("ENVD_TOKEN_HASH")); h != "" {
		return h
	}
	if b, err := os.ReadFile("/proc/cmdline"); err == nil {
		for _, tok := range strings.Fields(string(b)) {
			if v, ok := strings.CutPrefix(tok, "envd.token_hash="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// tokenAuthOptions returns the interceptors that enforce this sandbox's token.
// It never returns nil: a sandbox may boot with no credential and receive one later.
func tokenAuthOptions(hashes *hashStore) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.UnaryInterceptor(authUnary(hashes)),
		grpc.StreamInterceptor(authStream(hashes)),
		// Matches internal/envd.maxMsgBytes; the 4 MiB default refused every larger WriteFile.
		grpc.MaxRecvMsgSize(256 << 20),
	}
}

func authUnary(hashes *hashStore) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if authorized(info.FullMethod, ctx, hashes.Get()) {
			return handler(ctx, req)
		}
		return nil, errUnauthenticated
	}
}

func authStream(hashes *hashStore) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if authorized(info.FullMethod, ss.Context(), hashes.Get()) {
			return handler(srv, ss)
		}
		return errUnauthenticated
	}
}

// authorized reports whether a call may proceed. Health is always open;
// sbtoken.Verify refuses an empty wantHash.
func authorized(fullMethod string, ctx context.Context, wantHash string) bool {
	if strings.HasSuffix(fullMethod, "/Health") {
		return true
	}
	return sbtoken.Verify(tokenFromCtx(ctx), wantHash)
}

func tokenFromCtx(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(sbtoken.MetadataKey); len(v) > 0 {
		return v[0]
	}
	return ""
}
