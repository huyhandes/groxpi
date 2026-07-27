package logger

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	otellog "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// recordingExporter is the in-memory log exporter the SDK does not ship.
type recordingExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *recordingExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, records...)
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error   { return nil }
func (e *recordingExporter) ForceFlush(context.Context) error { return nil }

func (e *recordingExporter) collected() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

// restoreGlobals reverts the process-wide state that installing a logger
// provider and calling Init overwrite, so these tests do not leak into whatever
// runs after them.
func restoreGlobals(t *testing.T) {
	t.Helper()
	provider, global, def := otellog.GetLoggerProvider(), Logger, slog.Default()
	t.Cleanup(func() {
		otellog.SetLoggerProvider(provider)
		Logger = global
		slog.SetDefault(def)
	})
}

// TestLogRecordsCarryTraceContext is the correlation guarantee that motivated
// the logging-library swap: with a span active, the exported log record carries
// that span's trace and span identifiers, with no hand-rolled plumbing.
func TestLogRecordsCarryTraceContext(t *testing.T) {
	restoreGlobals(t)
	exporter := &recordingExporter{}
	otellog.SetLoggerProvider(sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
	))

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "request")
	_ = captureStdout(t, func() {
		// Init after the provider is installed: the bridge resolves it here.
		Init(LogConfig{Level: "INFO", Format: "json"})
		Logger.InfoContext(ctx, "handled", "package", "numpy")
	})
	span.End()

	records := exporter.collected()
	if len(records) != 1 {
		t.Fatalf("expected 1 exported log record, got %d", len(records))
	}
	rec := records[0]

	if rec.TraceID() != span.SpanContext().TraceID() {
		t.Errorf("trace id = %v, want %v", rec.TraceID(), span.SpanContext().TraceID())
	}
	if rec.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("span id = %v, want %v", rec.SpanID(), span.SpanContext().SpanID())
	}
	if body := rec.Body().AsString(); body != "handled" {
		t.Errorf("body = %q, want %q", body, "handled")
	}
	if !strings.EqualFold(rec.SeverityText(), "info") {
		t.Errorf("severity text = %q, want info", rec.SeverityText())
	}
}

// TestLogRecordsRespectLevelWhenBridged proves the configured level gates the
// OpenTelemetry bridge too, not just stdout.
func TestLogRecordsRespectLevelWhenBridged(t *testing.T) {
	restoreGlobals(t)
	exporter := &recordingExporter{}
	otellog.SetLoggerProvider(sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
	))

	_ = captureStdout(t, func() {
		Init(LogConfig{Level: "WARN", Format: "json"})
		Logger.Info("filtered out")
		Logger.Warn("kept")
	})

	records := exporter.collected()
	if len(records) != 1 {
		t.Fatalf("expected only the WARN record to be exported, got %d", len(records))
	}
	if body := records[0].Body().AsString(); body != "kept" {
		t.Errorf("body = %q, want %q", body, "kept")
	}
}
