package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Attribute keys carried by the metrics below. An index is always identified by
// its redacted URL — config.Index.Redacted or config.RedactURL at the call site,
// never the configured URL — because a metric attribute lives for as long as the
// collector keeps the series.
const (
	AttrCacheLayer     = "cache.layer"
	AttrIndex          = "index"
	AttrIndexResult    = "index.result"
	AttrRedirectReason = "redirect.reason"
	AttrFetchOutcome   = "fetch.outcome"
)

// Cache layers, the values of AttrCacheLayer.
const (
	LayerIndex  = "index"  // in-memory index cache
	LayerLocal  = "local"  // local object store
	LayerRemote = "remote" // remote object store
)

// Index resolution outcomes, the values of AttrIndexResult.
const (
	ResultHit       = "hit"
	ResultMiss      = "miss"
	ResultError     = "error"
	ResultCancelled = "cancelled" // a higher-priority index answered first
)

// Redirect reasons, the values of AttrRedirectReason. Every one of them is a
// request that was not cached, which is what makes this counter the one an
// operator watches to tell a too-tight time-to-first-byte budget from a working
// cache.
const (
	RedirectCachingDisabled = "caching_disabled" // no download budget configured
	RedirectFetchFailed     = "fetch_failed"     // budget exceeded, or upstream failed
	RedirectNotCached       = "not_cached"       // coordinated download left nothing cached
)

// Upstream fetch outcomes, the values of AttrFetchOutcome.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// The instruments are built once, at package initialisation, through the
// OpenTelemetry global meter: it delegates to whichever provider is installed
// later, and to nothing at all when none is. Construction errors are dropped
// because the names are compile-time constants and the returned instrument is
// safe to record on either way.
var (
	cacheHits      = counter("groxpi.cache.hits", "Cache lookups that found the object, by layer")
	cacheMisses    = counter("groxpi.cache.misses", "Cache lookups that did not find the object, by layer")
	cacheEvictions = counter("groxpi.cache.evictions", "Entries evicted or expired from a cache, by layer")
	indexResults   = counter("groxpi.index.resolutions", "Index resolution outcomes, by index and result")
	redirects      = counter("groxpi.redirects", "Requests answered with a redirect instead of a cached file, by reason")
	verifications  = counter("groxpi.verification.failures", "Downloads rejected because their bytes did not match the index")

	cacheBytes    = gauge("groxpi.cache.occupancy", "By", "Bytes currently held by a cache, by layer")
	fetchDuration = histogram("groxpi.upstream.fetch.duration", "s", "Duration of an upstream package file fetch")
)

func counter(name, description string) metric.Int64Counter {
	c, _ := Meter().Int64Counter(name, metric.WithDescription(description))
	return c
}

func gauge(name, unit, description string) metric.Int64Gauge {
	g, _ := Meter().Int64Gauge(name, metric.WithUnit(unit), metric.WithDescription(description))
	return g
}

func histogram(name, unit, description string) metric.Float64Histogram {
	h, _ := Meter().Float64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description))
	return h
}

func layerAttr(name string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String(AttrCacheLayer, name))
}

// CacheHit and CacheMiss count one lookup in one cache layer.
func CacheHit(ctx context.Context, layer string)  { cacheHits.Add(ctx, 1, layerAttr(layer)) }
func CacheMiss(ctx context.Context, layer string) { cacheMisses.Add(ctx, 1, layerAttr(layer)) }

// CacheEviction counts n entries dropped from a cache layer and CacheOccupancy
// reports what that layer holds. Together they are what makes a byte budget
// tunable against real traffic rather than guessed at.
func CacheEviction(ctx context.Context, layer string, n int64) {
	cacheEvictions.Add(ctx, n, layerAttr(layer))
}

func CacheOccupancy(ctx context.Context, layer string, bytes int64) {
	cacheBytes.Record(ctx, bytes, layerAttr(layer))
}

// IndexResolution counts one index's answer for one package. index must already
// be redacted.
func IndexResolution(ctx context.Context, index, result string) {
	indexResults.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrIndex, index),
		attribute.String(AttrIndexResult, result),
	))
}

// UpstreamFetch records how long an upstream package file fetch took.
func UpstreamFetch(ctx context.Context, d time.Duration, outcome string) {
	fetchDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String(AttrFetchOutcome, outcome)))
}

// Redirect counts a request sent upstream instead of being served from cache.
func Redirect(ctx context.Context, reason string) {
	redirects.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrRedirectReason, reason)))
}

// VerificationFailure counts a download whose bytes did not match what the index
// declared, and which was therefore not cached.
func VerificationFailure(ctx context.Context) { verifications.Add(ctx, 1) }
