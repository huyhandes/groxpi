package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestSetupWithoutEndpointIsInert is the regression guard against instrumentation
// becoming a startup dependency: with no collector endpoint, Setup installs
// nothing, cannot fail, and the tracer it hands instrumentation records nothing.
func TestSetupWithoutEndpointIsInert(t *testing.T) {
	shutdown, err := Setup(context.Background(), "", "groxpi")
	if err != nil {
		t.Fatalf("Setup with no endpoint must not fail: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Setup must always return a shutdown function")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown of inert telemetry must not fail: %v", err)
	}

	// Instrumentation still works, it just does nothing.
	_, span := Tracer().Start(context.Background(), "inert")
	span.End()
	if span.SpanContext().IsValid() {
		t.Error("expected a non-recording span with no provider configured")
	}
	if _, err := Meter().Int64Counter("groxpi.test.counter"); err != nil {
		t.Errorf("meter must be usable with no provider configured: %v", err)
	}
}

// TestSetupWithUnreachableEndpoint asserts that an endpoint nothing is listening
// on still returns cleanly: the OTLP/HTTP exporters connect lazily, so a
// collector that is down can neither block startup nor fail a request.
func TestSetupWithUnreachableEndpoint(t *testing.T) {
	shutdown, err := Setup(context.Background(), "http://127.0.0.1:1/v1/traces", "groxpi")
	if err != nil {
		t.Fatalf("Setup with an unreachable collector must not fail: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	ctx, span := Tracer().Start(context.Background(), "request")
	span.End()
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Error("expected a recording span once a provider is installed")
	}
}
