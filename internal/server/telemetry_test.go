package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Instrumentation resolves its tracer and its instruments through the
// OpenTelemetry globals, and the globals delegate exactly once per process. So
// the in-memory providers are installed once for the whole test binary, before
// any server in this file is constructed, and each test resets the span recorder
// and diffs the counters it cares about. Every other test file runs with no
// provider installed, which is the inert path production uses.
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

// snapshot reads every instrument once. Tests take one snapshot before and one
// after the traffic they generate and compare the two, because collecting is not
// free of side effects: a gauge sample is consumed by the collection that reads
// it.
type snapshot struct {
	rm metricdata.ResourceMetrics
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) snapshot {
	t.Helper()
	var s snapshot
	require.NoError(t, reader.Collect(context.Background(), &s.rm))
	return s
}

// value sums every data point of the named instrument whose attributes include
// want. Counters, the occupancy gauge and the fetch histogram are all reduced to
// one number: the assertions are about movement, not distribution.
func (s snapshot) value(name string, want ...attribute.KeyValue) int64 {
	rm := s.rm
	total := int64(0)
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					if hasAttrs(dp.Attributes, want) {
						total += dp.Value
					}
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					if hasAttrs(dp.Attributes, want) {
						total += dp.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					if hasAttrs(dp.Attributes, want) {
						total += int64(dp.Count)
					}
				}
			}
		}
	}
	return total
}

func hasAttrs(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		got, ok := set.Value(kv.Key)
		if !ok || got != kv.Value {
			return false
		}
	}
	return true
}

func layerAttr(layer string) attribute.KeyValue {
	return attribute.String(telemetry.AttrCacheLayer, layer)
}

// waitForSpan polls for a span, because the spans on the caching side of a
// download end after the response has been written.
func waitForSpan(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for range 200 {
		if span := findSpan(rec, name); span != nil {
			return span
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("span %q was never recorded", name)
	return nil
}

func findSpan(rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	for _, span := range rec.Ended() {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

func spansNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, span := range rec.Ended() {
		if span.Name() == name {
			out = append(out, span)
		}
	}
	return out
}

func assertChildOf(t *testing.T, child, parent sdktrace.ReadOnlySpan) {
	t.Helper()
	require.NotNil(t, child)
	require.NotNil(t, parent)
	assert.Equal(t, parent.SpanContext().SpanID(), child.Parent().SpanID(),
		"%s should be a child of %s", child.Name(), parent.Name())
}

// TestTelemetry_RequestSpanTree asserts the shape of a downloading request's
// trace: request, index resolution with a child per index, the storage existence
// check, the upstream fetch and the storage put underneath it.
func TestTelemetry_RequestSpanTree(t *testing.T) {
	rec, _ := installTestTelemetry(t)

	payload := testPayload(64 * 1024)
	pkg, file := "spantree", "spantree-1.0.0.tar.gz"
	up := NewFakePyPI(t).Add(pkg, file, FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) },
	})
	router, cacheDir := newCorrectnessServer(t, up.URL, 5*time.Second)

	resp := getFile(router, pkg, file)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, payload, readBody(t, resp))
	waitCached(t, cacheDir, pkg, file)

	put := waitForSpan(t, rec, "storage.put")
	request := findSpan(rec, "GET /index/{package}/{file}")
	require.NotNil(t, request, "the request span should be recorded")
	assert.False(t, request.Parent().IsValid(), "the request span is the root of the trace")

	resolve := findSpan(rec, "index.resolve")
	assertChildOf(t, resolve, request)
	assertChildOf(t, findSpan(rec, "index.query"), resolve)
	assertChildOf(t, findSpan(rec, "storage.exists"), request)

	fetch := findSpan(rec, "upstream.fetch")
	assertChildOf(t, fetch, request)
	assertChildOf(t, put, fetch)
}

// TestTelemetry_CacheHitAndMissCounters asserts the hit and miss counters move
// independently, per layer: the first request misses both the index cache and the
// local object store, the second hits both.
func TestTelemetry_CacheHitAndMissCounters(t *testing.T) {
	_, reader := installTestTelemetry(t)

	payload := testPayload(32 * 1024)
	pkg, file := "counters", "counters-1.0.0.tar.gz"
	up := NewFakePyPI(t).Add(pkg, file, FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) },
	})
	// An explicit index TTL, because an index cache entry stored with a zero TTL
	// expires the instant it is written and could never report a hit.
	cacheDir := t.TempDir()
	srv := newTestServer(t, &config.Config{
		IndexURL:        up.URL,
		CacheDir:        cacheDir,
		CacheSize:       1 << 30,
		IndexTTL:        time.Hour,
		IndexCacheSize:  1 << 20,
		DownloadTimeout: 5 * time.Second,
		LogLevel:        "ERROR",
	})
	t.Cleanup(func() { _ = srv.Close() })
	router := srv.Router()

	fetchedOK := attribute.String(telemetry.AttrFetchOutcome, telemetry.OutcomeOK)
	before := collect(t, reader)

	_ = readBody(t, getFile(router, pkg, file))
	waitCached(t, cacheDir, pkg, file)
	// The second file request is served from the object store without consulting
	// the index at all, so the index cache hit comes from the index page.
	_ = readBody(t, getFile(router, pkg, file))
	_ = getIndex(t, router, pkg)

	after := collect(t, reader)

	assert.Greater(t, after.value("groxpi.cache.misses", layerAttr(telemetry.LayerIndex)),
		before.value("groxpi.cache.misses", layerAttr(telemetry.LayerIndex)),
		"the cold request must count an index cache miss")
	assert.Greater(t, after.value("groxpi.cache.misses", layerAttr(telemetry.LayerLocal)),
		before.value("groxpi.cache.misses", layerAttr(telemetry.LayerLocal)),
		"the cold request must count a local object store miss")
	assert.Greater(t, after.value("groxpi.cache.hits", layerAttr(telemetry.LayerIndex)),
		before.value("groxpi.cache.hits", layerAttr(telemetry.LayerIndex)),
		"the warm request must count an index cache hit")
	assert.Greater(t, after.value("groxpi.cache.hits", layerAttr(telemetry.LayerLocal)),
		before.value("groxpi.cache.hits", layerAttr(telemetry.LayerLocal)),
		"the warm request must count a local object store hit")
	assert.Greater(t, after.value("groxpi.upstream.fetch.duration", fetchedOK),
		before.value("groxpi.upstream.fetch.duration", fetchedOK),
		"the cold request must record an upstream fetch duration")
	assert.Greater(t, after.value("groxpi.cache.occupancy", layerAttr(telemetry.LayerLocal)), int64(0),
		"the local object store must report its occupancy in bytes")
}

// TestTelemetry_RedirectCounter drives the header-delay fake: the budget is
// exceeded, the client is redirected, and the redirect is counted with a reason.
func TestTelemetry_RedirectCounter(t *testing.T) {
	_, reader := installTestTelemetry(t)

	payload := testPayload(32 * 1024)
	pkg, file := "redirectmetric", "redirectmetric-1.0.0.tar.gz"
	up := NewFakePyPI(t).Add(pkg, file, FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(600 * time.Millisecond)
			_, _ = w.Write(payload)
		},
	})
	router, _ := newCorrectnessServer(t, up.URL, 50*time.Millisecond)

	reason := attribute.String(telemetry.AttrRedirectReason, telemetry.RedirectFetchFailed)
	before := collect(t, reader)

	resp := getFile(router, pkg, file)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)

	assert.Greater(t, collect(t, reader).value("groxpi.redirects", reason),
		before.value("groxpi.redirects", reason),
		"a redirect must be counted with its reason")
}

// TestTelemetry_VerificationFailureCounter drives the sha256-mismatching fake: a
// rejected download is counted, so the integrity check cannot silently stop
// working again.
func TestTelemetry_VerificationFailureCounter(t *testing.T) {
	_, reader := installTestTelemetry(t)

	served := testPayload(32 * 1024)
	advertised := testPayload(32 * 1024)
	advertised[0] ^= 0xff
	pkg, file := "badhashmetric", "badhashmetric-1.0.0.tar.gz"
	up := NewFakePyPI(t).Add(pkg, file, FakeFile{SHA256: Sum(advertised), Size: int64(len(served)),
		Handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) },
	})
	router, _ := newCorrectnessServer(t, up.URL, 5*time.Second)

	before := collect(t, reader).value("groxpi.verification.failures")

	_ = readBody(t, getFile(router, pkg, file))

	assert.Eventually(t, func() bool {
		return collect(t, reader).value("groxpi.verification.failures") > before
	}, 2*time.Second, 20*time.Millisecond, "a rejected download must be counted")
}

// TestTelemetry_IndexResolutionIsRedacted is the security assertion for
// instrumentation: one child span per index consulted, each identified by the
// redacted URL, and the credential appears in no span attribute and no log field.
func TestTelemetry_IndexResolutionIsRedacted(t *testing.T) {
	rec, reader := installTestTelemetry(t)

	const secret = "sup3rs3cr3tpassw0rd"
	extra := newFakeUpstreamIndex(t, "extra", map[string][]string{"internal": {"internal-1.0.whl"}})
	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{"numpy": {"numpy-1.26.0.whl"}})

	logs := &lockedBuffer{}
	restore := captureLogs(t, logs)
	defer restore()

	extraURL := credentialedURL(t, extra.URL, "extrauser:"+secret)
	cfg := &config.Config{
		IndexURL:        credentialedURL(t, primary.URL, "primaryuser:"+secret),
		ExtraIndexURLs:  []string{extraURL},
		CacheDir:        t.TempDir(),
		IndexTTL:        time.Hour,
		IndexCacheSize:  1 << 20,
		DownloadTimeout: time.Second,
		LogLevel:        "DEBUG",
	}
	srv := newTestServer(t, cfg)
	t.Cleanup(func() { _ = srv.Close() })

	redactedExtra := config.RedactURL(extraURL)
	extraMissed := []attribute.KeyValue{
		attribute.String(telemetry.AttrIndex, redactedExtra),
		attribute.String(telemetry.AttrIndexResult, telemetry.ResultMiss),
	}
	before := collect(t, reader)

	// numpy is on the primary only, so both indexes are consulted.
	require.Equal(t, http.StatusOK, indexStatus(t, srv, "numpy"))

	queries := spansNamed(rec, "index.query")
	require.Len(t, queries, 2, "one child span per index consulted")
	for _, span := range queries {
		assertChildOf(t, span, findSpan(rec, "index.resolve"))
	}

	assert.Greater(t, collect(t, reader).value("groxpi.index.resolutions", extraMissed...),
		before.value("groxpi.index.resolutions", extraMissed...),
		"an index that missed must be counted as a miss under its redacted URL")

	for _, span := range rec.Ended() {
		for _, kv := range span.Attributes() {
			assert.NotContains(t, kv.Value.String(), secret,
				"span %s attribute %s carries the credential", span.Name(), kv.Key)
		}
	}
	assert.NotContains(t, logs.String(), secret, "credentials must appear in no log field")
	assert.True(t, strings.Contains(logs.String(), "redacted@"), "logs should show the redacted form")
}

// TestTelemetry_IndexCacheEvictionIsCounted asserts the index cache reports what
// it drops and what it holds — the two numbers the provisional byte budget is
// tuned against.
func TestTelemetry_IndexCacheEvictionIsCounted(t *testing.T) {
	_, reader := installTestTelemetry(t)

	primary := newFakeUpstreamIndex(t, "primary", map[string][]string{
		"one": {"one-1.0.whl"},
		"two": {"two-1.0.whl"},
	})
	indexLayer := layerAttr(telemetry.LayerIndex)
	before := collect(t, reader)

	// A cache with room reports what it holds.
	roomy := newMultiIndexServer(t, &config.Config{IndexCacheSize: 1 << 20}, primary)
	require.Equal(t, http.StatusOK, indexStatus(t, roomy, "one"))
	oneEntry := collect(t, reader).value("groxpi.cache.occupancy", indexLayer)
	assert.Greater(t, oneEntry, int64(0), "the index cache must report its occupancy in bytes")

	// A cache with room for exactly one entry evicts the first to fit the
	// second. (An entry bigger than the whole budget is not stored at all.)
	cramped := newMultiIndexServer(t, &config.Config{IndexCacheSize: oneEntry}, primary)
	require.Equal(t, http.StatusOK, indexStatus(t, cramped, "one"))
	require.Equal(t, http.StatusOK, indexStatus(t, cramped, "two"))

	assert.Greater(t, collect(t, reader).value("groxpi.cache.evictions", indexLayer),
		before.value("groxpi.cache.evictions", indexLayer),
		"an over-budget index cache must count its evictions")
}

// panickingRouter is the production middleware stack over a mux whose handlers
// panic, the plain way and the http.ErrAbortHandler way.
func panickingRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom/{name}", func(http.ResponseWriter, *http.Request) { panic("boom") })
	mux.HandleFunc("GET /abort/{name}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic(http.ErrAbortHandler)
	})
	mux.HandleFunc("GET /late-boom/{name}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("boom")
	})
	mux.HandleFunc("GET /mid-body-boom/{name}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "partial")
		_ = http.NewResponseController(w).Flush()
		panic("boom")
	})
	mux.HandleFunc("GET /early-abort/{name}", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	return recoverPanics(traceRequests(mux))
}

// TestTelemetry_PanickingRequestIsTracedAndLogged: a handler panic must not
// erase the request from the trace or the access log. The span is still named
// after its route and carries a 500 and an error status, the access line is
// still written, and the panic still reaches recoverPanics, which answers 500.
func TestTelemetry_PanickingRequestIsTracedAndLogged(t *testing.T) {
	rec, _ := installTestTelemetry(t)
	var logs strings.Builder
	defer captureLogs(t, &logs)()

	w := httptest.NewRecorder()
	panickingRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom/x", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	span := findSpan(rec, "GET /boom/{name}")
	require.NotNil(t, span, "the panicking request's span must be named after its route")
	assert.Contains(t, span.Attributes(), attribute.Int("http.response.status_code", http.StatusInternalServerError))
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "panic", span.Status().Description)

	assert.Contains(t, logs.String(), `"msg":"request"`)
	assert.Contains(t, logs.String(), `"status":500`)
	assert.Contains(t, logs.String(), `"path":"/boom/x"`)
}

// TestTelemetry_AbortedRequestIsMarked: http.ErrAbortHandler is net/http's
// deliberate abort, not a server error. The span and access line say aborted,
// keep the status already sent, and the abort still reaches net/http.
func TestTelemetry_AbortedRequestIsMarked(t *testing.T) {
	rec, _ := installTestTelemetry(t)
	var logs strings.Builder
	defer captureLogs(t, &logs)()

	func() {
		defer func() {
			assert.Equal(t, http.ErrAbortHandler, recover(), "the abort must be re-panicked")
		}()
		panickingRouter().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort/x", nil))
	}()

	span := findSpan(rec, "GET /abort/{name}")
	require.NotNil(t, span, "the aborted request's span must be named after its route")
	assert.Contains(t, span.Attributes(), attribute.Bool("http.request.aborted", true))
	assert.Contains(t, span.Attributes(), attribute.Int("http.response.status_code", http.StatusOK))
	assert.Equal(t, codes.Error, span.Status().Code)

	assert.Contains(t, logs.String(), `"aborted":true`)
	assert.Contains(t, logs.String(), `"path":"/abort/x"`)
}

// TestTelemetry_PanicAfterWriteKeepsSentStatus: a handler that panics after
// sending its status line cannot turn it into a 500. The span and access line
// record the status the client actually got; the span is still an error. The
// panic reaches net/http as an abort, so the connection is dropped rather than
// a 500 body appended to the response already under way.
func TestTelemetry_PanicAfterWriteKeepsSentStatus(t *testing.T) {
	rec, _ := installTestTelemetry(t)
	var logs strings.Builder
	defer captureLogs(t, &logs)()

	func() {
		defer func() {
			// The bare sentinel: net/http stays quiet on it, so the panic is
			// logged once, here, at ERROR, not by net/http through the INFO
			// log bridge.
			assert.Equal(t, http.ErrAbortHandler, recover(), "the panic must be re-raised as a bare abort")
		}()
		panickingRouter().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/late-boom/x", nil))
	}()

	span := findSpan(rec, "GET /late-boom/{name}")
	require.NotNil(t, span)
	assert.Contains(t, span.Attributes(), attribute.Int("http.response.status_code", http.StatusAccepted))
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "panic", span.Status().Description)
	assert.Contains(t, logs.String(), `"status":202`)
	assert.Contains(t, logs.String(), `"level":"ERROR","msg":"Handler panicked","panic":"boom"`)
	assert.Contains(t, logs.String(), `"stack":"goroutine`)
}

// TestTelemetry_AbortBeforeWriteClaimsNoStatus: an abort before anything was
// written sent the client no status, so neither the span nor the access line
// may claim a 200.
func TestTelemetry_AbortBeforeWriteClaimsNoStatus(t *testing.T) {
	rec, _ := installTestTelemetry(t)
	var logs strings.Builder
	defer captureLogs(t, &logs)()

	func() {
		defer func() { _ = recover() }()
		panickingRouter().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/early-abort/x", nil))
	}()

	span := findSpan(rec, "GET /early-abort/{name}")
	require.NotNil(t, span)
	for _, kv := range span.Attributes() {
		assert.NotEqual(t, attribute.Key("http.response.status_code"), kv.Key, "no status was sent")
	}
	assert.Contains(t, span.Attributes(), attribute.Bool("http.request.aborted", true))
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Contains(t, logs.String(), `"status":0`)
	assert.Contains(t, logs.String(), `"aborted":true`)
}

// TestPanicAfterBodyWriteDropsConnection: once part of a body has gone out, a
// 500 can no longer be sent, and appending "Internal Server Error" to a chunked
// body would hand the client a clean, complete-looking but wrong response. The
// connection must be dropped instead, so the client sees a broken body.
func TestPanicAfterBodyWriteDropsConnection(t *testing.T) {
	installTestTelemetry(t)
	var logs strings.Builder
	defer captureLogs(t, &logs)()

	srv := httptest.NewUnstartedServer(panickingRouter())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/mid-body-boom/x")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.Error(t, err, "a body cut short by a panic must not read as complete")
	assert.Equal(t, "partial", string(body))
}

// TestTelemetry_ServerErrorSetsSpanStatus: a 5xx response marks the request
// span as an error; a 4xx does not.
func TestTelemetry_ServerErrorSetsSpanStatus(t *testing.T) {
	rec, _ := installTestTelemetry(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bad", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	mux.HandleFunc("GET /missing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	h := traceRequests(mux)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bad", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))

	bad := findSpan(rec, "GET /bad")
	require.NotNil(t, bad)
	assert.Equal(t, codes.Error, bad.Status().Code)
	assert.Equal(t, http.StatusText(http.StatusBadGateway), bad.Status().Description)

	missing := findSpan(rec, "GET /missing")
	require.NotNil(t, missing)
	assert.Equal(t, codes.Unset, missing.Status().Code)
}
