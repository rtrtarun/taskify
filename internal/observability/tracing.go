package observability

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName names the tracer for every Taskify span.
const TracerName = "github.com/rtrtarun/taskify"

// Propagator is the W3C Trace Context propagator. It is used explicitly
// rather than through the global, so propagation works the same in tests,
// where no global is installed.
var Propagator = propagation.TraceContext{}

// Tracer returns Taskify's tracer from the global provider. Before
// SetupTracing runs it is a no-op tracer that still carries an incoming
// trace through, so trace IDs propagate even without an exporter.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// SetupTracing installs the global tracer provider and propagator.
//
// Spans are exported over OTLP/HTTP when OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set; the exporter reads its other
// settings from the standard OTEL_* variables. Without an endpoint, spans
// are still created, so requests get trace IDs that reach the outbox and
// AgentPulse, but nothing is exported. The returned function flushes and
// stops the provider.
func SetupTracing(ctx context.Context, version string) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("taskify"),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(Propagator, propagation.Baggage{}))
	return tp.Shutdown, nil
}

// InjectTraceContext returns the W3C trace headers for the span in ctx, for
// storing alongside an outbox row. It is empty when ctx carries no trace.
func InjectTraceContext(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	Propagator.Inject(ctx, carrier)
	return carrier
}

// ExtractTraceContext restores a trace stored by InjectTraceContext.
func ExtractTraceContext(ctx context.Context, stored map[string]string) context.Context {
	if len(stored) == 0 {
		return ctx
	}
	return Propagator.Extract(ctx, propagation.MapCarrier(stored))
}

// TraceID returns the trace ID in ctx, or "" when there is none.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}
