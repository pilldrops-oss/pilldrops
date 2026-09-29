package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveRequestIDRejectsUnsafeClientValues(t *testing.T) {
	for name, in := range map[string]string{
		"crlf injection":   "abc\r\nX-Evil: 1",
		"newline":          "abc\ndef",
		"space":            "has space",
		"slash":            "a/b",
		"query":            "a?b=c",
		"semicolon":        "a;b",
		"too long":         strings.Repeat("x", maxClientRequestID+1),
		"null byte":        "abc\x00def",
		"angle brackets":   "<script>",
		"percent encoding": "a%0d%0ab",
	} {
		got := ResolveRequestID(in)
		if got == in {
			t.Errorf("%s: echoed %q back verbatim", name, in)
		}
		if !isSafeID(got) {
			t.Errorf("%s: generated id %q is not URL-safe", name, got)
		}
		if !strings.HasPrefix(got, "req_") {
			t.Errorf("%s: want a freshly generated id, got %q", name, got)
		}
	}
}

func TestResolveRequestIDEchoesSafeClientValues(t *testing.T) {
	for _, in := range []string{
		"client-abc-123",
		"4bf92f3577b34da6a3ce929d0e0e4736",
		"a.b-c_d",
		strings.Repeat("x", maxClientRequestID),
	} {
		if got := ResolveRequestID(in); got != in {
			t.Errorf("ResolveRequestID(%q) = %q, want it echoed", in, got)
		}
	}
}

func TestResolveRequestIDGeneratesWhenAbsent(t *testing.T) {
	a, b := ResolveRequestID(""), ResolveRequestID("")
	if a == "" || b == "" {
		t.Fatal("want a generated id for an absent client value")
	}
	if a == b {
		t.Error("generated ids must differ between requests, or they cannot correlate anything")
	}
}

func TestRequestIDRoundTripsThroughContext(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req_abc")
	if got := RequestID(ctx); got != "req_abc" {
		t.Errorf("RequestID = %q, want req_abc", got)
	}
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("RequestID(bare ctx) = %q, want empty", got)
	}
}

func TestMiddlewarePutsTheIDInContextAndHeader(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "client-xyz")
	h.ServeHTTP(rec, req)

	if seen != "client-xyz" {
		t.Errorf("handler saw ctx id %q, want client-xyz", seen)
	}
	if got := rec.Header().Get(HeaderRequestID); got != "client-xyz" {
		t.Errorf("response header = %q, want client-xyz", got)
	}
}

func TestMiddlewareGeneratesWhenClientSendsNothing(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no id in context")
	}
	if rec.Header().Get(HeaderRequestID) != seen {
		t.Error("the header and the context must carry the same id")
	}
}

func TestPropagateCopiesTheIDOntoAnOutboundRequest(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req_shared")
	h := http.Header{}
	Propagate(ctx, h)
	if got := h.Get(HeaderRequestID); got != "req_shared" {
		t.Errorf("propagated %q, want req_shared", got)
	}

	// An empty header would make the next tier treat "" as a supplied value.
	bare := http.Header{}
	Propagate(context.Background(), bare)
	if _, ok := bare[http.CanonicalHeaderKey(HeaderRequestID)]; ok {
		t.Error("Propagate set the header with no id in context")
	}
}
