# Eager Span Evaluation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace quiescence-timer evaluation with per-batch eager evaluation, removing `buffer_ttl` and `interest_cache_ttl` config fields.

**Architecture:** Evaluation runs synchronously on each `ConsumeTraces` call before any buffer write. Interesting batches trigger immediate ingestion of buffered+current spans; uninteresting batches are buffered with a one-shot drop timer. `MutatesData` changed to `true` since the processor moves span ownership.

**Tech Stack:** Go, OpenTelemetry Collector processor SDK (`go.opentelemetry.io/collector`), `ptrace.Traces`, `time.AfterFunc`.

---

## File Map

| File | Change |
|---|---|
| `processor/retroactivesampling/config.go` | Remove `BufferTTL`, `InterestCacheTTL` fields |
| `processor/retroactivesampling/factory.go` | Remove removed field defaults; `MutatesData: false` → `true` |
| `processor/retroactivesampling/processor.go` | Remove `timers` map + `resetBufferTimer` + `onBufferTimeout`; add `ingestInteresting` + `startDropTimer`; `cache.New(cfg.DropTTL)` |
| `processor/retroactivesampling/processor_test.go` | Remove removed fields from `newTestProcessor`; update existing tests; add new tests |
| `processor/retroactivesampling/integration_test.go` | Remove removed fields from `newTestProcessor`; remove buffer_ttl sleeps |
| `example/otelcol1.yaml` | Remove `buffer_ttl`, `interest_cache_ttl` |
| `example/otelcol2.yaml` | Remove `buffer_ttl`, `interest_cache_ttl` |

---

### Task 1: Write failing test + fix config compilation

**Files:**
- Modify: `processor/retroactivesampling/config.go`
- Modify: `processor/retroactivesampling/factory.go`
- Modify: `processor/retroactivesampling/processor_test.go`
- Modify: `processor/retroactivesampling/integration_test.go`

- [ ] **Step 1: Add failing test `TestInterestingSpanIngestedWithoutDelay`**

Append to `processor/retroactivesampling/processor_test.go`, inside the file (after the existing tests):

```go
func TestInterestingSpanIngestedWithoutDelay(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	errTrace := makeTraceWithStatus("aabbccdd44444444aabbccdd44444444", ptrace.StatusCodeError)
	require.NoError(t, p.ConsumeTraces(context.Background(), errTrace))

	// Eager evaluation: interesting span ingested synchronously, no timer wait.
	assert.Equal(t, 1, sink.SpanCount())
}
```

- [ ] **Step 2: Run new test — verify it FAILS**

```bash
cd processor/retroactivesampling && go test -run TestInterestingSpanIngestedWithoutDelay -v -timeout 10s .
```

Expected: `FAIL` — `assert: 0 == 1` (current code waits for buffer_ttl before evaluating).

- [ ] **Step 3: Remove `BufferTTL` and `InterestCacheTTL` from `config.go`**

Replace the entire file:

```go
package processor

import (
	"time"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferDir           string                 `mapstructure:"buffer_dir"`
	DropTTL             time.Duration          `mapstructure:"drop_ttl"`
	MaxBufferBytes      int64                  `mapstructure:"max_buffer_bytes"`
	CoordinatorEndpoint string                 `mapstructure:"coordinator_endpoint"`
	Rules               []evaluator.RuleConfig `mapstructure:"rules"`
}
```

- [ ] **Step 4: Remove removed field defaults from `factory.go`**

Replace `createDefaultConfig`:

```go
func createDefaultConfig() component.Config {
	return &Config{
		DropTTL: 30 * time.Second,
	}
}
```

Leave `MutatesData` unchanged for now (Task 3 handles it).

- [ ] **Step 5: Fix compilation — remove `BufferTTL` and `InterestCacheTTL` from `processor_test.go`**

Replace `newTestProcessor` in `processor/retroactivesampling/processor_test.go`:

```go
func newTestProcessor(t *testing.T, addr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	cfg := &processor.Config{
		BufferDir:           t.TempDir(),
		DropTTL:             500 * time.Millisecond,
		CoordinatorEndpoint: addr,
		Rules: []evaluator.RuleConfig{
			{Type: "error_status"},
		},
	}
	factory := processor.NewFactory()
	p, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, p.Start(context.Background(), nil))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}
```

- [ ] **Step 6: Fix compilation — remove `BufferTTL` and `InterestCacheTTL` from `integration_test.go`**

Replace `newTestProcessor` in `processor/retroactivesampling/integration_test.go`:

```go
func newTestProcessor(t *testing.T, coordAddr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	cfg := &proc.Config{
		BufferDir:           t.TempDir(),
		DropTTL:             2 * time.Second,
		CoordinatorEndpoint: coordAddr,
		Rules:               []evaluator.RuleConfig{{Type: "error_status"}},
	}
	factory := proc.NewFactory()
	p, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, p.Start(context.Background(), nil))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}
```

- [ ] **Step 7: Verify partial compilation**

```bash
cd processor/retroactivesampling && go build ./... 2>&1 | grep -v "_test.go"
```

Expected: errors referencing `cfg.BufferTTL` and `cfg.InterestCacheTTL` in `processor.go` only. All other non-test packages compile. `processor.go` is fixed in Task 2.

- [ ] **Step 8: Commit**

```bash
git add processor/retroactivesampling/config.go \
        processor/retroactivesampling/factory.go \
        processor/retroactivesampling/processor_test.go \
        processor/retroactivesampling/integration_test.go
git commit -m "test: add eager evaluation test; remove buffer_ttl/interest_cache_ttl config fields"
```

---

### Task 2: Implement eager evaluation in `processor.go`

**Files:**
- Modify: `processor/retroactivesampling/processor.go`

- [ ] **Step 1: Replace `processor.go` entirely**

```go
package processor

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"
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

	mu    sync.Mutex
	drops map[string]*time.Timer
	wg    sync.WaitGroup
}

func newProcessor(logger *zap.Logger, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	buf, err := buffer.New(cfg.BufferDir, cfg.MaxBufferBytes)
	if err != nil {
		return nil, err
	}
	chain, err := evaluator.Build(cfg.Rules)
	if err != nil {
		buf.Close()
		return nil, err
	}
	p := &retroactiveProcessor{
		logger: logger,
		next:   next,
		buf:    buf,
		ic:     cache.New(cfg.DropTTL),
		eval:   chain,
		cfg:    cfg,
		drops:  make(map[string]*time.Timer),
	}
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision, logger)
	return p, nil
}

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	p.coord.Close()
	p.ic.Close()

	p.mu.Lock()
	for _, t := range p.drops {
		if t.Stop() {
			p.wg.Done()
		}
	}
	p.drops = make(map[string]*time.Timer)
	p.mu.Unlock()

	p.wg.Wait()
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
		p.startDropTimer(traceID)
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}

func (p *retroactiveProcessor) ingestInteresting(traceID string, current ptrace.Traces) {
	p.mu.Lock()
	if t, ok := p.drops[traceID]; ok {
		if t.Stop() {
			p.wg.Done()
		}
		delete(p.drops, traceID)
	}
	p.mu.Unlock()

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

func (p *retroactiveProcessor) startDropTimer(traceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.drops[traceID]; ok {
		return
	}
	p.wg.Add(1)
	p.drops[traceID] = time.AfterFunc(p.cfg.DropTTL, func() { defer p.wg.Done(); p.onDropTimeout(traceID) })
}

func (p *retroactiveProcessor) onDropTimeout(traceID string) {
	p.mu.Lock()
	delete(p.drops, traceID)
	p.mu.Unlock()
	_ = p.buf.Delete(traceID)
}

func (p *retroactiveProcessor) onDecision(traceID string, keep bool) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID), zap.Bool("keep", keep))
	p.mu.Lock()
	_, hadDropTimer := p.drops[traceID]
	if t, ok := p.drops[traceID]; ok {
		if t.Stop() {
			p.wg.Done()
		}
		delete(p.drops, traceID)
	}
	p.mu.Unlock()

	if !hadDropTimer {
		return
	}

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

- [ ] **Step 2: Verify compilation**

```bash
cd processor/retroactivesampling && go build ./...
```

Expected: no errors.

- [ ] **Step 3: Run the new failing test — verify it now passes**

```bash
cd processor/retroactivesampling && go test -run TestInterestingSpanIngestedWithoutDelay -v -timeout 10s .
```

Expected: `PASS`.

- [ ] **Step 4: Run all unit tests**

```bash
cd processor/retroactivesampling && go test -v -timeout 30s .
```

Expected: all tests pass. (`TestInterestingTraceIngestedImmediately` and `TestNonInterestingTraceDropped` may pass but with excess sleeps — cleaned up in Task 3.)

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/processor.go
git commit -m "feat: eager per-batch span evaluation, remove buffer_ttl timer"
```

---

### Task 3: Fix MutatesData + update existing unit tests + add new tests

**Files:**
- Modify: `processor/retroactivesampling/factory.go`
- Modify: `processor/retroactivesampling/processor_test.go`

- [ ] **Step 1: Change `MutatesData` to `true` in `factory.go`**

In `createTracesProcessor`, change:

```go
processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
```

to:

```go
processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}),
```

- [ ] **Step 2: Update `TestInterestingTraceIngestedImmediately` — remove buffer_ttl sleep**

Replace the test:

```go
func TestInterestingTraceIngestedImmediately(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	errTrace := makeTraceWithStatus("aabbccdd11111111aabbccdd11111111", ptrace.StatusCodeError)
	require.NoError(t, p.ConsumeTraces(context.Background(), errTrace))

	assert.Equal(t, 1, sink.SpanCount(), "error trace should be ingested")

	// Coordinator notification is async (gRPC send); give it a moment.
	time.Sleep(100 * time.Millisecond)
	fc.mu.Lock()
	notified := fc.notified
	fc.mu.Unlock()
	assert.NotEmpty(t, notified, "coordinator should be notified of interesting trace")
}
```

- [ ] **Step 3: Update `TestNonInterestingTraceDropped` — remove buffer_ttl sleep**

Replace the test:

```go
func TestNonInterestingTraceDropped(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	okTrace := makeTraceWithStatus("aabbccdd22222222aabbccdd22222222", ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))

	assert.Equal(t, 0, sink.SpanCount(), "ok span should be buffered, not ingested")

	// Wait for drop_ttl (500ms) to fire.
	time.Sleep(700 * time.Millisecond)

	assert.Equal(t, 0, sink.SpanCount(), "non-interesting trace should be dropped after drop_ttl")
}
```

- [ ] **Step 4: Update `TestCoordinatorPushCausesIngestion` — remove buffer_ttl wait comment**

Replace the test:

```go
func TestCoordinatorPushCausesIngestion(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid3 = "aabbccdd33333333aabbccdd33333333"
	okTrace := makeTraceWithStatus(tid3, ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))

	// ok span is not interesting: buffered immediately, nothing ingested.
	assert.Equal(t, 0, sink.SpanCount())

	// Coordinator signals: keep this trace.
	fc.sendDecision(tid3, true)
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1, sink.SpanCount(), "coordinator push should trigger ingestion")
}
```

- [ ] **Step 5: Add `TestEagerEval_BufferedSpansIncludedOnInterestingBatch`**

Append to `processor_test.go`:

```go
// TestEagerEval_BufferedSpansIncludedOnInterestingBatch: a non-interesting span is buffered first;
// a later interesting span for the same trace triggers ingestion of both.
func TestEagerEval_BufferedSpansIncludedOnInterestingBatch(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid = "aabbccdd55555555aabbccdd55555555"

	okTrace := makeTraceWithStatus(tid, ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))
	assert.Equal(t, 0, sink.SpanCount(), "ok span should be buffered")

	errTrace := makeTraceWithStatus(tid, ptrace.StatusCodeError)
	require.NoError(t, p.ConsumeTraces(context.Background(), errTrace))

	// Both spans (buffered ok + current error) ingested in one ConsumeTraces call.
	assert.Equal(t, 2, sink.SpanCount(), "buffered ok span and error span must both be ingested")
}
```

- [ ] **Step 6: Run all unit tests**

```bash
cd processor/retroactivesampling && go test -v -timeout 30s .
```

Expected: all tests pass.

- [ ] **Step 7: Commit**

```bash
git add processor/retroactivesampling/factory.go \
        processor/retroactivesampling/processor_test.go
git commit -m "test: update unit tests for eager evaluation; set MutatesData=true"
```

---

### Task 4: Update integration test

**Files:**
- Modify: `processor/retroactivesampling/integration_test.go`

- [ ] **Step 1: Remove the buffer_ttl sleep from `TestE2E_ErrorPropagatesAcrossTwoProcessors`**

Replace the test body (keep function signature and helpers):

```go
func TestE2E_ErrorPropagatesAcrossTwoProcessors(t *testing.T) {
	coordAddr := startCoordinator(t)

	sinkA := &consumertest.TracesSink{}
	sinkB := &consumertest.TracesSink{}
	procA := newTestProcessor(t, coordAddr, sinkA)
	procB := newTestProcessor(t, coordAddr, sinkB)

	traceIDHex := "aabbccddeeff00112233445566778899"

	// B receives ok span — buffered immediately (not interesting).
	require.NoError(t, procB.ConsumeTraces(context.Background(), spanWithStatus(traceIDHex, ptrace.StatusCodeOk)))
	assert.Equal(t, 0, sinkB.SpanCount(), "ok span should be buffered on B")

	// A receives error span — ingested immediately and coordinator notified.
	require.NoError(t, procA.ConsumeTraces(context.Background(), spanWithStatus(traceIDHex, ptrace.StatusCodeError)))
	assert.Equal(t, 1, sinkA.SpanCount(), "processor A should ingest its error span immediately")

	// Budget for gRPC broadcast + B onDecision: 1s gives margin on slow CI.
	time.Sleep(1000 * time.Millisecond)

	assert.Equal(t, 1, sinkB.SpanCount(), "processor B should ingest its buffered span after coordinator push")
}
```

- [ ] **Step 2: Run integration test**

```bash
cd processor/retroactivesampling && go test -tags=integration -v -timeout 30s .
```

Expected: `PASS`.

- [ ] **Step 3: Commit**

```bash
git add processor/retroactivesampling/integration_test.go
git commit -m "test: update integration test for eager evaluation"
```

---

### Task 5: Update example configs

**Files:**
- Modify: `example/otelcol1.yaml`
- Modify: `example/otelcol2.yaml`

- [ ] **Step 1: Remove `buffer_ttl` and `interest_cache_ttl` from `example/otelcol1.yaml`**

Replace the `retroactive_sampling` section:

```yaml
processors:
  retroactive_sampling:
    buffer_dir: ./retrosampling1
    drop_ttl: 30s
    max_buffer_bytes: 1048576  # 1 MB
    coordinator_endpoint: localhost:9090
    rules:
      - type: error_status
      - type: high_latency
        threshold: 5s
```

- [ ] **Step 2: Remove `buffer_ttl` and `interest_cache_ttl` from `example/otelcol2.yaml`**

Replace the `retroactive_sampling` section:

```yaml
processors:
  retroactive_sampling:
    buffer_dir: ./retrosampling2
    drop_ttl: 30s
    max_buffer_bytes: 1048576  # 1 MB
    coordinator_endpoint: localhost:9090
    rules:
      - type: error_status
      - type: high_latency
        threshold: 5s
```

- [ ] **Step 3: Update `processor/retroactivesampling/README.md` config table**

Replace the config table:

```markdown
| Key | Required | Default | Description |
|---|---|---|---|
| `buffer_dir` | yes | — | Directory for per-trace buffer files |
| `max_buffer_bytes` | no | `0` (unlimited) | Max bytes of buffer disk usage; oldest traces evicted first |
| `drop_ttl` | no | `30s` | Time to wait for a coordinator keep signal before dropping |
| `coordinator_endpoint` | yes | — | `host:port` of coordinator gRPC server |
| `rules` | yes | — | List of sampling rules (evaluated with OR logic) |
```

Also update step 2 in the "How it works" section:

```markdown
2. Each incoming batch of spans is evaluated immediately against configured rules.
```

- [ ] **Step 4: Run full test suite one final time**

```bash
cd processor/retroactivesampling && go test -v -timeout 30s . && go test -tags=integration -v -timeout 30s .
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add example/otelcol1.yaml example/otelcol2.yaml processor/retroactivesampling/README.md
git commit -m "docs: remove buffer_ttl and interest_cache_ttl from examples and README"
```
