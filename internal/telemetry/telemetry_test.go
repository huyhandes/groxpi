package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otelapilog "go.opentelemetry.io/otel/log"
	otellog "go.opentelemetry.io/otel/log/global"
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
	shutdown, err := Setup(context.Background(), "http://127.0.0.1:1", "groxpi")
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

// TestSetupPostsEachSignalToItsOTLPPath pins the documented contract that
// OTEL_EXPORTER_OTLP_ENDPOINT is a collector base URL: each signal is posted
// to <base>/v1/traces, /v1/metrics and /v1/logs, as an OTel collector expects.
func TestSetupPostsEachSignalToItsOTLPPath(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]bool{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.URL.Path] = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	shutdown, err := Setup(context.Background(), collector.URL, "groxpi")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_, span := Tracer().Start(context.Background(), "request")
	span.End()
	counter, err := Meter().Int64Counter("groxpi.test.counter")
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	counter.Add(context.Background(), 1)
	var rec otelapilog.Record
	rec.SetBody(attribute.StringValue("hello"))
	otellog.GetLoggerProvider().Logger(ScopeName).Emit(context.Background(), rec)

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		if !paths[want] {
			t.Errorf("collector never received a POST to %s; got %v", want, paths)
		}
	}
}
