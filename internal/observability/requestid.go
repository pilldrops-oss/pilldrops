// Package observability holds the logging, request-id and tracing primitives
// shared by the control plane and the nodes.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderRequestID is the header carrying the correlation id on every tier.
const HeaderRequestID = "X-Request-ID"

type ctxKey struct{}

// WithRequestID returns a context carrying id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the correlation id carried by ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

const maxClientRequestID = 128

// ResolveRequestID returns the client's own id when it is short and URL-safe,
// otherwise a fresh one. The value lands in a response header and in logs, so
// an unvalidated one is a header-injection and log-forging vector.
func ResolveRequestID(fromClient string) string {
	if n := len(fromClient); n > 0 && n <= maxClientRequestID && isSafeID(fromClient) {
		return fromClient
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// A request must never fail for want of an id.
		return ""
	}
	return "req_" + hex.EncodeToString(b)
}

func isSafeID(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// Middleware resolves the request id, echoes it, and puts it in the request context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ResolveRequestID(r.Header.Get(HeaderRequestID))
		if id != "" {
			w.Header().Set(HeaderRequestID, id)
			r = r.WithContext(WithRequestID(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

// Propagate copies the correlation id from ctx onto an outbound request.
func Propagate(ctx context.Context, h http.Header) {
	if id := RequestID(ctx); id != "" {
		h.Set(HeaderRequestID, id)
	}
}
