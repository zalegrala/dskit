// Provenance-includes-location: https://github.com/weaveworks/common/blob/main/tracing/tracing.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: Weaveworks Ltd.

package tracing

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	jaeger "github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	jaegerprom "github.com/uber/jaeger-lib/metrics/prometheus"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ErrInvalidConfiguration is an error to notify client to provide valid trace report agent or config server
var (
	ErrBlankTraceConfiguration = errors.New("no trace report agent, config server, or collector endpoint specified")
)

// installJaeger registers Jaeger as the OpenTracing implementation.
func installJaeger(serviceName string, cfg *jaegercfg.Configuration, options ...jaegercfg.Option) (io.Closer, error) {
	metricsFactory := jaegerprom.New()

	// put the metricsFactory earlier so provided options can override it
	opts := append([]jaegercfg.Option{jaegercfg.Metrics(metricsFactory)}, options...)

	closer, err := cfg.InitGlobalTracer(serviceName, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "could not initialize jaeger tracer")
	}
	return closer, nil
}

// NewFromEnv is a convenience function to allow tracing configuration
// via environment variables
//
// Tracing will be enabled if one (or more) of the following environment variables is used to configure trace reporting:
// - JAEGER_AGENT_HOST
// - JAEGER_SAMPLER_MANAGER_HOST_PORT
func NewFromEnv(serviceName string, options ...jaegercfg.Option) (io.Closer, error) {
	cfg, err := jaegercfg.FromEnv()
	if err != nil {
		return nil, errors.Wrap(err, "could not load jaeger tracer configuration")
	}

	if cfg.Sampler.SamplingServerURL == "" && cfg.Reporter.LocalAgentHostPort == "" && cfg.Reporter.CollectorEndpoint == "" {
		return nil, ErrBlankTraceConfiguration
	}

	return installJaeger(serviceName, cfg, options...)
}

// ExtractTraceID extracts the trace id, if any from the context.
func ExtractTraceID(ctx context.Context) (string, bool) {
	sp := opentracing.SpanFromContext(ctx)
	if sp == nil {
		return "", false
	}
	sctx, ok := sp.Context().(jaeger.SpanContext)
	if !ok {
		return "", false
	}

	return sctx.TraceID().String(), true
}

// ExtractSampledTraceID works like ExtractTraceID but the returned bool is only
// true if the returned trace id is sampled.
func ExtractSampledTraceID(ctx context.Context) (string, bool) {
	sp := opentracing.SpanFromContext(ctx)
	if sp == nil {
		return "", false
	}
	sctx, ok := sp.Context().(jaeger.SpanContext)
	if !ok {
		return "", false
	}

	return sctx.TraceID().String(), sctx.IsSampled()
}

// InstallOpenTelemetryTracer initializes an OpenTelemetry tracer.
func InstallOpenTelemetryTracer(config Config, logger *slog.Logger, appName, version string) (func(), error) {
	if config.OtelEndpoint == "" {
		return func() {}, nil
	}

	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}

	logger.Info("initializing OpenTelemetry tracer", "endpoint", config.OtelEndpoint)

	ctx := context.Background()

	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(appName),
			semconv.ServiceVersionKey.String(version),
		),
		resource.WithHost(),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize trace resuorce")
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		/* grpc.WithBlock(), */
	}

	conn, err := grpc.NewClient(config.OtelEndpoint, dialOpts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to dial otel grpc")
	}

	options := []otlptracegrpc.Option{otlptracegrpc.WithGRPCConn(conn)}
	if config.OrgID != "" {
		options = append(options, otlptracegrpc.WithHeaders(map[string]string{"X-Scope-OrgID": config.OrgID}))
	}

	traceExporter, err := otlptracegrpc.New(ctx, options...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create trace exporter")
	}

	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)

	// set global propagator to tracecontext (the default is no-op).
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(propagation.Baggage{}, propagation.TraceContext{}),
	)

	otel.SetTracerProvider(tracerProvider)

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracerProvider.Shutdown(ctx); err != nil {
			logger.Error("OpenTelemetry trace provider failed to shutdown", "err", err)
			os.Exit(1)
		}
	}

	/* if config.InstallOTBridge { */
	/* 	otelTracer := tracerProvider.Tracer("tracer_name") */
	/* 	// Use the bridgeTracer as your OpenTracing tracer. */
	/* 	bridgeTracer, wrapperTracerProvider := otelBridge.NewTracerPair(otelTracer) */
	/* 	// Set the wrapperTracerProvider as the global OpenTelemetry */
	/* 	// TracerProvider so instrumentation will use it by default. */
	/* 	otel.SetTracerProvider(wrapperTracerProvider) */
	/* } else { */
	/* 	otel.SetTracerProvider(tracerProvider) */
	/* } */

	return shutdown, nil
}
