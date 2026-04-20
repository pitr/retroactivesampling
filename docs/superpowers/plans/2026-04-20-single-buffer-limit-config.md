# Single Buffer Limit Config Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `drop_ttl` + `max_buffer_bytes` with a single `max_buffer_bytes` limit, converting `InterestCache` from TTL-based to LRU count-based, and exposing actual trace retention time as an OTel histogram metric.

**Architecture:** Buffer eviction fires an `onEvict` callback (synchronous, under buffer lock) that deletes the trace from the LRU `InterestCache` and records retention duration. The `drops` map and all drop timer logic in the processor are removed. Config gains `MaxInterestCacheEntries` (default 100,000); `DropTTL` is removed.

**Tech Stack:** Go, `container/list` (stdlib LRU), `go.opentelemetry.io/otel/metric` (histogram), OTel Collector processor SDK.

---

## File Map

| File | Change |
|---|---|
| `processor/retroactivesampling/internal/cache/cache.go` | Rewrite: TTL+sweep goroutine → LRU count-bounded |
| `processor/retroactivesampling/internal/cache/cache_test.go` | Replace TTL tests with LRU behaviour tests |
| `processor/retroactivesampling/internal/buffer/buffer.go` | Add `onEvict func(string, time.Time)` callback |
| `processor/retroactivesampling/internal/buffer/buffer_test.go` | Add nil to `New` calls; add callback test |
| `processor/retroactivesampling/config.go` | Remove `DropTTL`; add `MaxInterestCacheEntries int` |
| `processor/retroactivesampling/factory.go` | Update default config; pass `set.TelemetrySettings` |
| `processor/retroactivesampling/processor.go` | Remove `drops`/`wg`/timers; wire callback + metric |
| `processor/retroactivesampling/processor_test.go` | Remove `DropTTL` field; delete `TestNonInterestingTraceDropped` |
| `processor/retroactivesampling/integration_test.go` | Remove `DropTTL` field |
| `processor/retroactivesampling/README.md` | Update config table and example |

---

## Task 1: Rewrite InterestCache as LRU

**Files:**
- Modify: `processor/retroactivesampling/internal/cache/cache_test.go`
- Modify: `processor/retroactivesampling/internal/cache/cache.go`

- [ ] **Step 1: Replace cache_test.go with LRU tests**

```go
package cache_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/cache"
)

func TestHasReturnsTrueAfterAdd(t *testing.T) {
	c := cache.New(10)
	c.Add("trace-1")
	assert.True(t, c.Has("trace-1"))
}

func TestHasReturnsFalseForUnknown(t *testing.T) {
	c := cache.New(10)
	assert.False(t, c.Has("trace-unknown"))
}

func TestDeleteRemovesEntry(t *testing.T) {
	c := cache.New(10)
	c.Add("trace-1")
	c.Delete("trace-1")
	assert.False(t, c.Has("trace-1"))
}

func TestDeleteNonExistentIsNoOp(t *testing.T) {
	c := cache.New(10)
	c.Delete("trace-unknown") // must not panic
}

func TestLRUEvictsOldestWhenAtCapacity(t *testing.T) {
	c := cache.New(2)
	c.Add("trace-1")
	c.Add("trace-2")
	c.Add("trace-3") // evicts trace-1
	assert.False(t, c.Has("trace-1"), "oldest entry should be evicted")
	assert.True(t, c.Has("trace-2"))
	assert.True(t, c.Has("trace-3"))
}

func TestHasUpdatesLRUPosition(t *testing.T) {
	c := cache.New(2)
	c.Add("trace-1")
	c.Add("trace-2")
	c.Has("trace-1")  // moves trace-1 to front; trace-2 becomes LRU tail
	c.Add("trace-3")  // evicts trace-2
	assert.True(t, c.Has("trace-1"), "recently accessed entry should survive")
	assert.False(t, c.Has("trace-2"), "LRU tail should be evicted")
	assert.True(t, c.Has("trace-3"))
}

func TestAddExistingMovesToFront(t *testing.T) {
	c := cache.New(2)
	c.Add("trace-1")
	c.Add("trace-2")
	c.Add("trace-1") // re-add moves to front; trace-2 is now LRU tail
	c.Add("trace-3") // evicts trace-2
	assert.True(t, c.Has("trace-1"))
	assert.False(t, c.Has("trace-2"), "trace-2 should be evicted as LRU")
	assert.True(t, c.Has("trace-3"))
}
```

- [ ] **Step 2: Run tests — expect compile errors or failures**

```bash
cd processor/retroactivesampling && go test ./internal/cache/... -v
```

Expected: compile errors (`cache.New` signature mismatch, `Delete` undefined, `Close` called nowhere but sweep goroutine still runs).

- [ ] **Step 3: Replace cache.go with LRU implementation**

```go
package cache

import (
	"container/list"
	"sync"
)

type InterestCache struct {
	mu      sync.Mutex
	cap     int
	entries map[string]*list.Element
	lru     list.List
}

func New(capacity int) *InterestCache {
	return &InterestCache{
		cap:     capacity,
		entries: make(map[string]*list.Element),
	}
}

func (c *InterestCache) Add(traceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[traceID]; ok {
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(traceID)
	c.entries[traceID] = el
	if c.lru.Len() > c.cap {
		back := c.lru.Back()
		c.lru.Remove(back)
		delete(c.entries, back.Value.(string))
	}
}

func (c *InterestCache) Has(traceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[traceID]
	if !ok {
		return false
	}
	c.lru.MoveToFront(el)
	return true
}

func (c *InterestCache) Delete(traceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[traceID]; ok {
		c.lru.Remove(el)
		delete(c.entries, traceID)
	}
}
```

- [ ] **Step 4: Run tests — expect PASS**

```bash
cd processor/retroactivesampling && go test ./internal/cache/... -v
```

Expected: all 7 tests pass.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/cache/
git commit -m "refactor(cache): replace TTL sweep with LRU count-bounded InterestCache"
```

---

## Task 2: Add Eviction Callback to SpanBuffer

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 1: Add eviction callback test to buffer_test.go**

Add after `TestWriteWithEvictionBytesLimit`:

```go
func TestWriteWithEvictionFiresCallback(t *testing.T) {
	sample := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(sample)
	require.NoError(t, err)
	traceFileSize := int64(8 + len(data))

	var evictedID string
	var evictedAt time.Time
	buf, err := buffer.New(t.TempDir(), traceFileSize, func(id string, insertedAt time.Time) {
		evictedID = id
		evictedAt = insertedAt
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	insertTime := time.Now()
	trA := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	require.NoError(t, buf.WriteWithEviction("traceA", trA, insertTime))

	trB := singleSpanTraces("traceB_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	require.NoError(t, buf.WriteWithEviction("traceB", trB, insertTime.Add(time.Millisecond)))

	assert.Equal(t, "traceA", evictedID, "callback should receive evicted trace ID")
	assert.WithinDuration(t, insertTime, evictedAt, time.Second, "callback should receive original insertedAt")
}
```

- [ ] **Step 2: Run test — expect compile error**

```bash
cd processor/retroactivesampling && go test ./internal/buffer/... -v -run TestWriteWithEvictionFiresCallback
```

Expected: compile error — `buffer.New` takes 2 args, not 3.

- [ ] **Step 3: Update buffer.go — add onEvict field and update New signature**

Change the `SpanBuffer` struct (add field after `totalBytes`):

```go
type SpanBuffer struct {
	dir      string
	maxBytes int64

	mu         sync.Mutex
	entries    map[string]entry
	totalBytes int64
	onEvict    func(traceID string, insertedAt time.Time)
}
```

Change `New`:

```go
func New(dir string, maxBytes int64, onEvict func(string, time.Time)) (*SpanBuffer, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	b := &SpanBuffer{dir: dir, maxBytes: maxBytes, onEvict: onEvict, entries: make(map[string]entry)}
	return b, b.loadExisting()
}
```

Change `evictOldestLocked` — capture `oldestTime` before deletion and fire callback after:

```go
func (b *SpanBuffer) evictOldestLocked(skipID string) string {
	oldest, oldestTime := "", time.Time{}
	for id, e := range b.entries {
		if id == skipID {
			continue
		}
		if oldest == "" || e.insertedAt.Before(oldestTime) {
			oldest, oldestTime = id, e.insertedAt
		}
	}
	if oldest == "" {
		return ""
	}
	if b.deleteLocked(oldest) != nil {
		return ""
	}
	if b.onEvict != nil {
		b.onEvict(oldest, oldestTime)
	}
	return oldest
}
```

- [ ] **Step 4: Fix the two existing `New` calls in buffer_test.go — add nil as third arg**

In `newTestBuffer`:
```go
func newTestBuffer(t *testing.T) *buffer.SpanBuffer {
	t.Helper()
	buf, err := buffer.New(t.TempDir(), 0, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}
```

In `TestWriteWithEvictionBytesLimit`:
```go
buf, err := buffer.New(t.TempDir(), traceFileSize, nil) // capacity for exactly one trace
```

- [ ] **Step 5: Run all buffer tests — expect PASS**

```bash
cd processor/retroactivesampling && go test ./internal/buffer/... -v
```

Expected: all tests pass including `TestWriteWithEvictionFiresCallback`.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/
git commit -m "feat(buffer): add onEvict callback fired synchronously on eviction"
```

---

## Task 3: Update Config, Factory, and Processor

**Files:**
- Modify: `processor/retroactivesampling/config.go`
- Modify: `processor/retroactivesampling/factory.go`
- Modify: `processor/retroactivesampling/processor.go`

- [ ] **Step 1: Replace config.go**

```go
package processor

import (
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferDir               string                 `mapstructure:"buffer_dir"`
	MaxBufferBytes          int64                  `mapstructure:"max_buffer_bytes"`
	MaxInterestCacheEntries int                    `mapstructure:"max_interest_cache_entries"`
	CoordinatorEndpoint     string                 `mapstructure:"coordinator_endpoint"`
	Rules                   []evaluator.RuleConfig `mapstructure:"rules"`
}
```

- [ ] **Step 2: Replace factory.go**

```go
package processor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	otelprocessor "go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

const typeStr = "retroactive_sampling"

func NewFactory() otelprocessor.Factory {
	return otelprocessor.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		otelprocessor.WithTraces(createTracesProcessor, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		MaxInterestCacheEntries: 100_000,
	}
}

func createTracesProcessor(
	ctx context.Context,
	set otelprocessor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (otelprocessor.Traces, error) {
	p, err := newProcessor(set.TelemetrySettings, cfg.(*Config), next)
	if err != nil {
		return nil, err
	}
	return processorhelper.NewTraces(ctx, set, cfg, next, p.processTraces,
		processorhelper.WithShutdown(p.Shutdown),
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}),
	)
}
```

- [ ] **Step 3: Replace processor.go**

```go
package processor

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/cache"
	coord "pitr.ca/retroactivesampling/processor/retroactivesampling/internal/coordinator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type retroactiveProcessor struct {
	logger *zap.Logger
	next   consumer.Traces
	buf    *buffer.SpanBuffer
	ic     *cache.InterestCache
	eval   evaluator.Evaluator
	coord  *coord.Client
	cfg    *Config
}

func newProcessor(set component.TelemetrySettings, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	ic := cache.New(cfg.MaxInterestCacheEntries)
	meter := set.MeterProvider.Meter("retroactive_sampling")
	retentionHist, err := meter.Float64Histogram(
		"retroactive_sampling.trace_retention_seconds",
		metric.WithDescription("Time a trace spent in the buffer before eviction"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	buf, err := buffer.New(cfg.BufferDir, cfg.MaxBufferBytes, func(traceID string, insertedAt time.Time) {
		ic.Delete(traceID)
		retentionHist.Record(context.Background(), time.Since(insertedAt).Seconds())
	})
	if err != nil {
		return nil, err
	}
	chain, err := evaluator.Build(cfg.Rules)
	if err != nil {
		buf.Close()
		return nil, err
	}
	p := &retroactiveProcessor{
		logger: set.Logger,
		next:   next,
		buf:    buf,
		ic:     ic,
		eval:   chain,
		cfg:    cfg,
	}
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision, set.Logger)
	return p, nil
}

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	p.coord.Close()
	return p.buf.Close()
}

func (p *retroactiveProcessor) processTraces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	out := ptrace.NewTraces()
	for traceID, spans := range groupByTrace(td) {
		if p.ic.Has(traceID) {
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		if p.eval.Evaluate(spans) {
			p.ingestInteresting(traceID, spans)
			continue
		}
		if err := p.buf.WriteWithEviction(traceID, spans, time.Now()); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", traceID), zap.Error(err))
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}

func (p *retroactiveProcessor) ingestInteresting(traceID string, current ptrace.Traces) {
	p.ic.Add(traceID)
	p.coord.Notify(traceID)

	buffered, ok, err := p.buf.Read(traceID)
	if err != nil {
		p.logger.Warn("read buffer for interesting trace", zap.String("trace_id", traceID), zap.Error(err))
		if err2 := p.next.ConsumeTraces(context.Background(), current); err2 != nil {
			p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err2))
		}
		_ = p.buf.Delete(traceID)
		return
	}
	if ok {
		current.ResourceSpans().MoveAndAppendTo(buffered.ResourceSpans())
		current = buffered
	}
	if err := p.next.ConsumeTraces(context.Background(), current); err != nil {
		p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err))
	}
	_ = p.buf.Delete(traceID)
}

func (p *retroactiveProcessor) onDecision(traceID string, keep bool) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID), zap.Bool("keep", keep))
	if !keep {
		_ = p.buf.Delete(traceID)
		return
	}
	p.ic.Add(traceID)
	traces, ok, err := p.buf.Read(traceID)
	if err != nil {
		p.logger.Warn("coordinator said keep but got error fetching it", zap.String("trace_id", traceID), zap.Error(err))
		return
	}
	if !ok {
		return
	}
	if err := p.next.ConsumeTraces(context.Background(), traces); err != nil {
		p.logger.Error("ingest coordinator-decided trace", zap.String("trace_id", traceID), zap.Error(err))
	}
	_ = p.buf.Delete(traceID)
}
```

- [ ] **Step 4: Verify compilation**

```bash
cd processor/retroactivesampling && go build ./...
```

Expected: success. If there are import errors (e.g. `go.opentelemetry.io/otel/metric` not in go.sum), run:
```bash
go get go.opentelemetry.io/otel/metric && go mod tidy
```

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/config.go \
        processor/retroactivesampling/factory.go \
        processor/retroactivesampling/processor.go
git commit -m "feat(processor): remove drop_ttl; add LRU interest cache and retention metric"
```

---

## Task 4: Fix Tests

**Files:**
- Modify: `processor/retroactivesampling/processor_test.go`
- Modify: `processor/retroactivesampling/integration_test.go`

- [ ] **Step 1: Update `newTestProcessor` in processor_test.go — remove `DropTTL`, add `MaxInterestCacheEntries`**

Replace the `cfg` literal in `newTestProcessor`:

```go
cfg := &processor.Config{
    BufferDir:               t.TempDir(),
    MaxInterestCacheEntries: 1000,
    CoordinatorEndpoint:     addr,
    Rules: []evaluator.RuleConfig{
        {Type: "error_status"},
    },
}
```

- [ ] **Step 2: Delete `TestNonInterestingTraceDropped` from processor_test.go**

Remove the entire function (lines 133–147 in the original):

```go
func TestNonInterestingTraceDropped(t *testing.T) { ... }
```

This test relied on `drop_ttl` firing a timer. The behaviour no longer exists: non-interesting traces are evicted by buffer pressure, not by time.

- [ ] **Step 3: Remove unused `"time"` import from processor_test.go if it is now unused**

Check whether `time` is still referenced (it is used in `makeTraceWithStatus` and `require.Eventually` timeouts). If still referenced, leave it. If not, remove.

- [ ] **Step 4: Update `newE2EProcessor` in integration_test.go — remove `DropTTL`**

Replace the `cfg` literal:

```go
cfg := &proc.Config{
    BufferDir:               t.TempDir(),
    MaxInterestCacheEntries: 1000,
    CoordinatorEndpoint:     coordAddr,
    Rules:                   []evaluator.RuleConfig{{Type: "error_status"}},
}
```

- [ ] **Step 5: Run unit tests — expect PASS**

```bash
cd processor/retroactivesampling && go test ./... -v -count=1
```

Expected: all tests pass. `TestNonInterestingTraceDropped` is gone; remaining tests unchanged.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/processor_test.go \
        processor/retroactivesampling/integration_test.go
git commit -m "test: remove drop_ttl from test configs; delete timer-based drop test"
```

---

## Task 5: Update README

**Files:**
- Modify: `processor/retroactivesampling/README.md`

- [ ] **Step 1: Read README to locate sections to update**

```bash
cat processor/retroactivesampling/README.md
```

- [ ] **Step 2: Update the config table**

Remove the `drop_ttl` row. Add `max_interest_cache_entries`:

| Config | Required | Default | Description |
|---|---|---|---|
| `buffer_dir` | yes | — | Directory for buffered trace files |
| `max_buffer_bytes` | no | `0` (unlimited) | Max bytes of buffer disk usage; oldest traces evicted first |
| `max_interest_cache_entries` | no | `100000` | Max number of interesting trace IDs cached in memory for fast-path routing |
| `coordinator_endpoint` | yes | — | gRPC address of the coordinator |
| `rules` | yes | — | List of sampling rules |

- [ ] **Step 3: Update the example config — remove `drop_ttl`, add `max_interest_cache_entries`**

Replace the YAML example to remove `drop_ttl: 30s` and add (if not already present):
```yaml
max_interest_cache_entries: 100000  # optional, default shown
```

- [ ] **Step 4: Update any prose that references `drop_ttl`**

Search for occurrences:
```bash
grep -n drop_ttl processor/retroactivesampling/README.md
```

Replace each reference. Example: "schedule drop after `drop_ttl`" → "evicted when `max_buffer_bytes` is exceeded (oldest first)".

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/README.md
git commit -m "docs: update README for single buffer limit config (remove drop_ttl)"
```
