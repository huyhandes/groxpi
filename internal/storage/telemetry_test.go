package storage

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The OpenTelemetry globals delegate exactly once per process, so the in-memory
// providers are installed once for the storage test binary. Storage tests do
// not run in parallel, so a test may diff the instruments around its traffic.
var (
	testTelemetryOnce sync.Once
	spanRecorder      *tracetest.SpanRecorder
	metricReader      *sdkmetric.ManualReader
)

func installTestTelemetry(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	testTelemetryOnce.Do(func() {
		spanRecorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder)))
		metricReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader)))
	})
	spanRecorder.Reset()
	return spanRecorder, metricReader
}

// fetchDuration sums the count and the seconds of every upstream-fetch sample.
func fetchDuration(t *testing.T, reader *sdkmetric.ManualReader) (count uint64, seconds float64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == "groxpi.upstream.fetch.duration" {
				for _, dp := range h.DataPoints {
					count += dp.Count
					seconds += dp.Sum
				}
			}
		}
	}
	return count, seconds
}

func TestStore_FetchMetricExcludesTheUpload(t *testing.T) {
	const putDelay = 300 * time.Millisecond
	spans, reader := installTestTelemetry(t)
	count0, sum0 := fetchDuration(t, reader)

	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	remote, bucket := newFakeS3(t)
	bucket.putGate, bucket.putSeen = make(chan struct{}), make(chan struct{})
	f := &fakeFetcher{body: bytes.Repeat([]byte("u"), 100000)}
	s := NewStore(l1, remote, f, "", false)

	obj, err := s.Open(context.Background(), "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err)
	body, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.NoError(t, obj.Close())
	assert.Equal(t, f.body, body)

	select {
	case <-bucket.putSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never reached the bucket")
	}
	count, _ := fetchDuration(t, reader)
	assert.Equal(t, count0+1, count, "the fetch is recorded before the upload finishes")
	time.Sleep(putDelay) // the slow PUT the metric must not include
	close(bucket.putGate)
	require.NoError(t, s.Close())

	count, sum := fetchDuration(t, reader)
	require.Equal(t, count0+1, count)
	assert.Less(t, sum-sum0, putDelay.Seconds(), "the fetch duration excludes the PUT")
	assert.Equal(t, f.body, bucket.object(mustKey(t, "pkg", "pkg-1.0.tar.gz")))

	var upload, flight sdktrace.ReadOnlySpan
	for _, sp := range spans.Ended() {
		switch sp.Name() {
		case "storage.upload":
			upload = sp
		case "upstream.fetch":
			flight = sp
		}
	}
	require.NotNil(t, upload, "the PUT has its own span")
	require.NotNil(t, flight)
	assert.Equal(t, flight.SpanContext().SpanID(), upload.Parent().SpanID(), "storage.upload is a child of the flight span")
}
