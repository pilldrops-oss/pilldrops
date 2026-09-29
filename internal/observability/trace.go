package observability

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Spans are created only at inbound HTTP, the control-plane-to-node hop, daemon
// lifecycle transitions and store queries. Without an endpoint Init installs a
// no-op provider.

const tracerName = "github.com/pilldrops-oss/pilldrops"

// Tracer returns the process tracer. Safe before Init; the global provider starts as a no-op.
func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

// TraceConfig configures the exporter.
type TraceConfig struct {
	// Endpoint is an OTLP/HTTP collector host:port. Empty disables tracing.
	Endpoint string
	// Insecure sends plaintext OTLP.
	Insecure bool
	// ServiceName names this process in the trace.
	ServiceName string
	// SampleRatio is the head sampling ratio, 0..1.
	SampleRatio float64
}

// TraceConfigFromEnv reads the standard OTEL_* variables.
func TraceConfigFromEnv(serviceName string, ratio float64) TraceConfig {
	ep := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	return TraceConfig{
		Endpoint:    strings.TrimSuffix(ep, "/"),
		Insecure:    strings.EqualFold(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), "true"),
		ServiceName: serviceName,
		SampleRatio: ratio,
	}
}

// Init installs the global tracer provider and returns a shutdown function,
// always non-nil. Call it on exit or the batched final spans are dropped.
func Init(ctx context.Context, cfg TraceConfig) (shutdown func(context.Context) error, err error) {
	noopShutdown := func(context.Context) error { return nil }
	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return noopShutdown, nil
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return noopShutdown, fmt.Errorf("otlp exporter: %w", err)
	}

	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		// ParentBased so the edge's sampling decision holds across tiers; otherwise
		// the node can sample out half of a trace the control plane sampled in.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(serviceResource(cfg.ServiceName)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// TraceMiddleware starts a server span per inbound request, continuing any
// trace the caller began. Wrap it outside the request-id middleware. route must
// return the matched pattern, not the raw path, or every id is its own operation.
func TraceMiddleware(route func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(),
				propagation.HeaderCarrier(r.Header))

			name := r.Method
			if route != nil {
				if p := route(r); p != "" {
					name = r.Method + " " + p
				}
			}
			ctx, span := Tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer))
			defer span.End()

			if id := RequestID(ctx); id != "" {
				span.SetAttributes(attribute.String("request_id", id))
			}
			span.SetAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.target", r.URL.Path),
			)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// StartServer begins the inbound-request span. route is the matched pattern, not the raw path.
func StartServer(ctx context.Context, method, route string) (context.Context, trace.Span) {
	name := method
	if route != "" {
		name = method + " " + route
	}
	ctx, span := Tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer))
	span.SetAttributes(
		attribute.String("http.method", method),
		attribute.String("http.route", route),
	)
	if id := RequestID(ctx); id != "" {
		span.SetAttributes(attribute.String("request_id", id))
	}
	return ctx, span
}

// SetHTTPStatus records the response status and marks 5xx as an error.
func SetHTTPStatus(span trace.Span, status int) {
	span.SetAttributes(attribute.Int("http.status_code", status))
	if status >= 500 {
		span.SetStatus(codes.Error, http.StatusText(status))
	}
}

// InjectTrace writes the current trace context onto an outbound request.
func InjectTrace(ctx context.Context, h http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
}

// StartInternal begins a span around an operation inside this process.
func StartInternal(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// End finishes a span, recording err when non-nil.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// TraceID returns the current trace id, or "" when not tracing.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

func serviceResource(name string) *resource.Resource {
	if name == "" {
		name = "pilldrops"
	}
	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(name),
	)
}

// shutdownTimeout keeps a hung collector from holding up process exit.
const shutdownTimeout = 5 * time.Second

// Flush drains pending spans with a bounded deadline.
func Flush(shutdown func(context.Context) error) {
	if shutdown == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = shutdown(ctx)
}

// TraceSampleRatio reads OTEL_TRACES_SAMPLER_ARG, defaulting to 1.
func TraceSampleRatio() float64 {
	v := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
	if v == "" {
		return 1
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1 {
		return 1
	}
	return f
}
