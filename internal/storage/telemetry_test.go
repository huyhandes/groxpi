package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// cacheMisses sums the storage cache-miss counter over every layer.
func cacheMisses(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var n int64
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "groxpi.cache.misses" {
				for _, dp := range s.DataPoints {
					n += dp.Value
				}
			}
		}
	}
	return n
}

// TestStore_RetriedLookupCountsOneMiss: when a commit in the same commit slot
// makes Open look a second time, the second lookup's miss is not counted again.
func TestStore_RetriedLookupCountsOneMiss(t *testing.T) {
	keyA, err := Key("a", "a-1.0.tar.gz")
	require.NoError(t, err)
	keyB, err := Key("b10", "b10-1.0.tar.gz")
	require.NoError(t, err)
	require.Equal(t, commitSlot(keyA), commitSlot(keyB), "the keys must share a slot")

	_, reader := installTestTelemetry(t)
	held, release := make(chan struct{}), make(chan struct{})
	var lookups atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Count(r.URL.Path, "/") < 2 || r.Method == http.MethodPut {
			return // bucket check, upload
		}
		if strings.HasSuffix(r.URL.Path, "/a-1.0.tar.gz") && lookups.Add(1) == 1 {
			close(held) // a's first lookup waits until b has committed
			<-release
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	remote, err := NewS3Storage(&S3Config{Endpoint: srv.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	s := NewStore(nil, remote, &fakeFetcher{body: []byte("bytes")}, t.TempDir(), false)

	before := cacheMisses(t, reader)
	late := make(chan error, 1)
	go func() {
		obj, err := s.Open(t.Context(), "a", "a-1.0.tar.gz")
		if err == nil {
			_, err = io.ReadAll(obj)
			_ = obj.Close()
		}
		late <- err
	}()
	<-held

	obj, err := s.Open(t.Context(), "b10", "b10-1.0.tar.gz")
	require.NoError(t, err)
	_, err = io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	require.Eventually(t, func() bool { return len(s.InFlight()) == 0 }, 5*time.Second, time.Millisecond,
		"b commits, so a's Open looks again")
	close(release)
	require.NoError(t, <-late)
	require.NoError(t, s.Close())

	assert.Equal(t, int64(2), lookups.Load(), "a was looked up twice")
	assert.Equal(t, before+2, cacheMisses(t, reader), "one miss per Open: a once, b once")
}
