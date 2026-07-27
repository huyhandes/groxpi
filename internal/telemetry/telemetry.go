// Package telemetry installs the process-wide OpenTelemetry providers for all
// three signals — traces, metrics and logs — exported over OTLP to a single
// collector endpoint.
//
// Instrumentation reaches these providers through the OpenTelemetry globals
// ([Tracer], [Meter]) rather than through an injected seam. With no endpoint
// configured [Setup] installs nothing: the globals stay no-op, no connection is
// attempted at startup, and every instrumentation call is a cheap no-op. An
// unreachable collector is absorbed by the exporters' own retry and drop
// behaviour, so it can neither block startup nor fail a request.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope every groxpi tracer, meter and log
// bridge is registered under.
const ScopeName = "github.com/huyhandes/groxpi"

// Tracer returns the tracer instrumentation should use. It is safe to call
// before [Setup]: the OpenTelemetry global delegates to the real provider once
// one is installed.
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Meter returns the meter instrumentation should use. Same delegation rules as
// [Tracer].
func Meter() metric.Meter { return otel.Meter(ScopeName) }

// Setup installs the global providers and returns a shutdown function that
// flushes and releases them. An empty endpoint leaves telemetry inert and
// returns a shutdown that does nothing.
func Setup(ctx context.Context, endpoint, serviceName string) (shutdown func(context.Context) error, err error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build telemetry resource: %w", err)
	}

	traceExp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}
	metricExp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP metric exporter: %w", err)
	}
	logExp, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
	)
	lp := log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(logExp)),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otellog.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// Export failures are reported, never propagated: a collector that is down
	// must not turn into a failed request.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("telemetry export error", "error", err)
	}))

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}, nil
}
