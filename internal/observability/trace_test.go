package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recording installs an in-memory span recorder and restores the previous provider after.
func recording(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

func TestTracingIsANoOpWhenUnconfigured(t *testing.T) {
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	shutdown, err := Init(context.Background(), TraceConfig{})
	if err != nil {
		t.Fatalf("Init with no endpoint should succeed: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must be non-nil so callers can defer it unconditionally")
	}
	_, span := StartInternal(context.Background(), "should-not-record")
	if span.IsRecording() {
		t.Error("span is recording with no exporter configured")
	}
	if span.SpanContext().IsValid() {
		t.Error("no-op provider produced a valid span context")
	}
	span.End()
	Flush(shutdown)
}

func TestTraceContinuesAcrossTheProxyHop(t *testing.T) {
	sr := recording(t)

	upstreamCtx, upstreamSpan := StartServer(context.Background(), http.MethodPost, "/v1/sandboxes")
	wantTrace := upstreamSpan.SpanContext().TraceID()

	outbound := http.Header{}
	InjectTrace(upstreamCtx, outbound)
	if outbound.Get("traceparent") == "" {
		t.Fatal("no traceparent injected — the node cannot join this trace")
	}

	var nodeTrace trace.TraceID
	var sawParent bool
	handler := TraceMiddleware(func(*http.Request) string { return "/v1/sandboxes" })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sc := trace.SpanContextFromContext(r.Context())
			nodeTrace, sawParent = sc.TraceID(), sc.IsValid()
		}))

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", nil)
	for k, vs := range outbound {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	upstreamSpan.End()

	if !sawParent {
		t.Fatal("the node's span context is invalid — extraction failed")
	}
	if nodeTrace != wantTrace {
		t.Errorf("node trace %s != control-plane trace %s — the hop started a NEW trace, which is the failure this guards",
			nodeTrace, wantTrace)
	}
	if len(sr.Ended()) == 0 {
		t.Error("no spans recorded")
	}
}

func TestServerSpanIsNamedByRouteNotPath(t *testing.T) {
	sr := recording(t)
	_, span := StartServer(context.Background(), http.MethodGet, "/v1/sandboxes/{id}")
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	name := ended[0].Name()
	if name != "GET /v1/sandboxes/{id}" {
		t.Errorf("span name = %q, want the route pattern", name)
	}
}

func TestServerSpanCarriesTheRequestID(t *testing.T) {
	sr := recording(t)
	ctx := WithRequestID(context.Background(), "req_correlate")
	_, span := StartServer(ctx, http.MethodGet, "/health")
	span.End()

	for _, a := range sr.Ended()[0].Attributes() {
		if string(a.Key) == "request_id" {
			if a.Value.AsString() != "req_correlate" {
				t.Errorf("request_id = %q", a.Value.AsString())
			}
			return
		}
	}
	t.Error("span carries no request_id attribute")
}

func TestServerSpanMarksServerErrors(t *testing.T) {
	sr := recording(t)
	_, span := StartServer(context.Background(), http.MethodGet, "/x")
	SetHTTPStatus(span, http.StatusInternalServerError)
	span.End()
	if got := sr.Ended()[0].Status().Code.String(); got != "Error" {
		t.Errorf("status = %s, want Error for a 5xx", got)
	}

	sr2 := recording(t)
	_, ok := StartServer(context.Background(), http.MethodGet, "/x")
	SetHTTPStatus(ok, http.StatusNotFound)
	ok.End()
	if got := sr2.Ended()[0].Status().Code.String(); got == "Error" {
		t.Error("a 404 was marked as a server error")
	}
}

func TestEndRecordsErrors(t *testing.T) {
	sr := recording(t)
	_, span := StartInternal(context.Background(), "op")
	End(span, context.DeadlineExceeded)

	ended := sr.Ended()[0]
	if ended.Status().Code.String() != "Error" {
		t.Error("failed span not marked as an error")
	}
	if len(ended.Events()) == 0 {
		t.Error("the error was not recorded as a span event")
	}
}

func TestTraceIDIsEmptyWhenNotTracing(t *testing.T) {
	if got := TraceID(context.Background()); got != "" {
		t.Errorf("TraceID = %q for a bare context, want empty", got)
	}
}

func TestTraceConfigFromEnvNormalisesTheEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"http://collector:4318":  "collector:4318",
		"https://collector:4318": "collector:4318",
		"collector:4318/":        "collector:4318",
		"":                       "",
	} {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", in)
		if got := TraceConfigFromEnv("svc", 1).Endpoint; got != want {
			t.Errorf("endpoint %q normalised to %q, want %q", in, got, want)
		}
	}
}
