# Design: Consolidate Buffer Limit to Single Config

**Date:** 2026-04-20

## Problem

The processor has two configs that both limit buffer growth:

- `drop_ttl` — drops a trace after N time if no coordinator "keep" signal arrives
- `max_buffer_bytes` — evicts oldest traces when disk usage exceeds a threshold

These are not truly equivalent: `drop_ttl` is a time budget per trace; `max_buffer_bytes` is a global disk cap. Crucially, `drop_ttl` cannot be guaranteed when `max_buffer_bytes` forces early eviction. Having both configs implies a guarantee we cannot keep.

## Decision

Remove `drop_ttl`. Keep `max_buffer_bytes` as the single buffer bound. Replace the TTL-based `InterestCache` with an LRU count-based cache, configured via a new `max_interest_cache_entries` param. Report actual trace retention duration as an OTel histogram metric.

## Design

### Config

```go
type Config struct {
    BufferDir               string                 `mapstructure:"buffer_dir"`
    MaxBufferBytes          int64                  `mapstructure:"max_buffer_bytes"`
    MaxInterestCacheEntries int                    `mapstructure:"max_interest_cache_entries"`
    CoordinatorEndpoint     string                 `mapstructure:"coordinator_endpoint"`
    Rules                   []evaluator.RuleConfig `mapstructure:"rules"`
}
```

- `drop_ttl` removed.
- `MaxInterestCacheEntries` added; default `100_000`. Traces remain in the IC until evicted by LRU pressure or explicitly deleted.
- `MaxBufferBytes` default unchanged (`0` = unlimited).

### Buffer (`internal/buffer/buffer.go`)

`SpanBuffer` gains an `onEvict func(traceID string, insertedAt time.Time)` field, set at construction:

```go
func New(dir string, maxBytes int64, onEvict func(string, time.Time)) (*SpanBuffer, error)
```

`evictOldestLocked` captures `insertedAt` before deletion, then calls `onEvict(traceID, insertedAt)` synchronously after. The callback runs under `mu` — it must not call back into the buffer.

### InterestCache (`internal/cache/cache.go`)

Rewritten from TTL+sweep goroutine to count-bounded LRU using `container/list` + map:

```go
func New(capacity int) *InterestCache
func (c *InterestCache) Add(traceID string)        // insert or move-to-front; evict tail if over cap
func (c *InterestCache) Has(traceID string) bool   // move-to-front on hit
func (c *InterestCache) Delete(traceID string)     // explicit removal
// Close() removed — no background goroutine
```

`Has` updates LRU position so actively-used traces are not evicted. `Delete` enables the buffer eviction callback to remove the evicted trace from IC immediately.

### Factory (`factory.go`)

- Remove `DropTTL: 30 * time.Second` from `createDefaultConfig`.
- Add `MaxInterestCacheEntries: 100_000` to `createDefaultConfig`.
- Pass `set` (full `otelprocessor.Settings`) to `newProcessor` instead of just `set.Logger`.

### Processor (`processor.go`)

**Removed:**
- `drops map[string]*time.Timer`
- `wg sync.WaitGroup`
- `startDropTimer`, `onDropTimeout`
- All timer stop/cancel logic in `ingestInteresting` and `Shutdown`

**Wiring in `newProcessor`:**

```go
ic := cache.New(cfg.MaxInterestCacheEntries)
meter := set.TelemetrySettings.MeterProvider().Meter("retroactive_sampling")
retentionHist, _ := meter.Float64Histogram(
    "retroactive_sampling.trace_retention_seconds",
    metric.WithDescription("Time a trace spent in the buffer before eviction"),
    metric.WithUnit("s"),
)
buf, err := buffer.New(cfg.BufferDir, cfg.MaxBufferBytes, func(traceID string, insertedAt time.Time) {
    ic.Delete(traceID)
    retentionHist.Record(context.Background(), time.Since(insertedAt).Seconds())
})
```

`newProcessor` gains a `set otelprocessor.Settings` parameter (was `logger *zap.Logger` only).

**`Shutdown` simplifies to:**
```go
func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
    p.coord.Close()
    return p.buf.Close()
}
```

No timer drain needed.

### Retention Metric

| Field | Value |
|---|---|
| Name | `retroactive_sampling.trace_retention_seconds` |
| Type | Float64 histogram |
| Unit | `s` |
| Recorded when | A trace is evicted from the buffer (not when it is kept/dropped by coordinator) |
| Value | `time.Since(insertedAt).Seconds()` at eviction time |

This metric reflects only eviction-driven retention (buffer pressure). Traces removed by coordinator decision do not contribute — they represent the happy path, not the bound.

## Behavioral Change

Without `drop_ttl`, non-interesting traces that receive no coordinator decision are only removed by buffer eviction. In low-traffic scenarios (buffer never fills), such traces accumulate until new traffic causes eviction. This is acceptable: low traffic means plentiful disk, and the coordinator should eventually decide. If this becomes a concern, a background sweep can be added later.
