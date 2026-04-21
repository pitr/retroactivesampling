# Double-Write Fix: Concurrent Interesting Spans Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix double-write of buffered spans when two goroutines concurrently decide the same trace is interesting.

**Architecture:** Add `ReadAndDelete` to `SpanBuffer` — atomically reads and removes the entry in one mutex acquisition. Replace the separate `Read`+`Delete` pairs in `ingestInteresting` and `onDecision` with this method. The concurrent loser gets `ok=false` and forwards only its current spans; the winner merges the buffered spans. No span is ever delivered twice.

**Tech Stack:** Go, `sync.Mutex`, `go.opentelemetry.io/collector/pdata/ptrace`, `github.com/stretchr/testify`

---

## Files

- Modify: `processor/retroactivesampling/internal/buffer/buffer.go` — add `ReadAndDelete`
- Test: `processor/retroactivesampling/internal/buffer/buffer_test.go` — unit tests for `ReadAndDelete`
- Modify: `processor/retroactivesampling/processor.go` — use `ReadAndDelete` in `ingestInteresting` and `onDecision`
- Test: `processor/retroactivesampling/processor_test.go` — concurrent integration tests

---

### Task 1: Add `ReadAndDelete` to `SpanBuffer`

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`
- Test: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 1: Write failing buffer unit tests**

Append to `processor/retroactivesampling/internal/buffer/buffer_test.go`:

```go
func TestReadAndDelete(t *testing.T) {
	buf := newBuf(t)
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	require.NoError(t, buf.WriteWithEviction(traceA, tr, time.Now()))

	got, ok, err := buf.ReadAndDelete(traceA)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())

	// Second call: entry gone.
	_, ok, err = buf.ReadAndDelete(traceA)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestReadAndDeleteMissing(t *testing.T) {
	buf := newBuf(t)
	_, ok, err := buf.ReadAndDelete(traceA)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestReadAndDeleteMultipleDeltas(t *testing.T) {
	buf := newBuf(t)
	t1 := singleSpanTraces(traceA, ptrace.StatusCodeOk, 50)
	t2 := singleSpanTraces(traceA, ptrace.StatusCodeOk, 60)
	require.NoError(t, buf.WriteWithEviction(traceA, t1, time.Now()))
	require.NoError(t, buf.WriteWithEviction(traceA, t2, time.Now()))

	got, ok, err := buf.ReadAndDelete(traceA)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.SpanCount())

	_, ok, _ = buf.ReadAndDelete(traceA)
	assert.False(t, ok)
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run 'TestReadAndDelete' -v
```

Expected: `FAIL` — `buf.ReadAndDelete undefined`.

- [ ] **Step 3: Implement `ReadAndDelete` in `buffer.go`**

Add after the `Read` method (line 233 in `buffer.go`):

```go
func (b *SpanBuffer) ReadAndDelete(traceID string) (ptrace.Traces, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	deltas := b.entries[traceID]
	if len(deltas) == 0 {
		return ptrace.Traces{}, false, nil
	}
	delete(b.entries, traceID)

	u := ptrace.ProtoUnmarshaler{}
	result := ptrace.NewTraces()
	for _, d := range deltas {
		buf := make([]byte, d.size)
		copy(buf, b.data[d.offset+hdrSize:])
		t, err := u.UnmarshalTraces(buf)
		if err != nil {
			return ptrace.Traces{}, false, err
		}
		t.ResourceSpans().MoveAndAppendTo(result.ResourceSpans())
	}
	return result, true, nil
}
```

- [ ] **Step 4: Run buffer tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -v -race
```

Expected: all `PASS`, no race warnings.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go \
        processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "feat(buffer): add ReadAndDelete for atomic read+evict"
```

---

### Task 2: Use `ReadAndDelete` in the processor

**Files:**
- Modify: `processor/retroactivesampling/processor.go`

- [ ] **Step 1: Replace `Read`+`Delete` in `ingestInteresting`**

Current `ingestInteresting` (lines 92–113):

```go
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
```

Replace with:

```go
func (p *retroactiveProcessor) ingestInteresting(traceID string, current ptrace.Traces) {
	p.ic.Add(traceID)
	p.coord.Notify(traceID)

	buffered, ok, err := p.buf.ReadAndDelete(traceID)
	if err != nil {
		p.logger.Warn("read buffer for interesting trace", zap.String("trace_id", traceID), zap.Error(err))
		if err2 := p.next.ConsumeTraces(context.Background(), current); err2 != nil {
			p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err2))
		}
		return
	}
	if ok {
		current.ResourceSpans().MoveAndAppendTo(buffered.ResourceSpans())
		current = buffered
	}
	if err := p.next.ConsumeTraces(context.Background(), current); err != nil {
		p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err))
	}
}
```

- [ ] **Step 2: Replace `Read`+`Delete` in `onDecision`**

Current `onDecision` (lines 115–130):

```go
func (p *retroactiveProcessor) onDecision(traceID string) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID))
	p.ic.Add(traceID)
	traces, ok, err := p.buf.Read(traceID)
	if err != nil {
		p.logger.Warn("coordinator decision: error fetching buffered trace", zap.String("trace_id", traceID), zap.Error(err))
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

Replace with:

```go
func (p *retroactiveProcessor) onDecision(traceID string) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID))
	p.ic.Add(traceID)
	traces, ok, err := p.buf.ReadAndDelete(traceID)
	if err != nil {
		p.logger.Warn("coordinator decision: error fetching buffered trace", zap.String("trace_id", traceID), zap.Error(err))
		return
	}
	if !ok {
		return
	}
	if err := p.next.ConsumeTraces(context.Background(), traces); err != nil {
		p.logger.Error("ingest coordinator-decided trace", zap.String("trace_id", traceID), zap.Error(err))
	}
}
```

- [ ] **Step 3: Build to confirm no compilation errors**

```
cd processor/retroactivesampling && go build ./...
```

Expected: no output, exit 0.

- [ ] **Step 4: Run existing processor tests**

```
cd processor/retroactivesampling && go test ./... -v -race
```

Expected: all existing tests `PASS`, no race warnings.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/processor.go
git commit -m "fix(processor): use ReadAndDelete to prevent double-write on concurrent interesting spans"
```

---

### Task 3: Add concurrent processor tests

**Files:**
- Test: `processor/retroactivesampling/processor_test.go`

- [ ] **Step 1: Add `TestConcurrentInterestingSpansSameTrace`**

Append to `processor/retroactivesampling/processor_test.go`:

```go
// TestConcurrentInterestingSpansSameTrace verifies no double-write when two goroutines
// concurrently decide the same trace is interesting.
func TestConcurrentInterestingSpansSameTrace(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid = "aabbccdd66666666aabbccdd66666666"

	// Pre-buffer one ok span.
	require.NoError(t, p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeOk)))
	assert.Equal(t, 0, sink.SpanCount(), "ok span should be buffered, not ingested")

	// Release two goroutines simultaneously, each delivering an error span for the same trace.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(2)
	for range 2 {
		go func() {
			start.Wait()
			_ = p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeError))
			done.Done()
		}()
	}
	start.Done()
	done.Wait()

	// 1 pre-buffered ok span + 2 error spans = 3. The buffered span must not appear twice (4).
	assert.Equal(t, 3, sink.SpanCount(), "buffered span must not be duplicated")
}
```

- [ ] **Step 2: Add `TestConcurrentInterestingAndCoordinatorDecision`**

Append after the previous test:

```go
// TestConcurrentInterestingAndCoordinatorDecision verifies no double-write when
// ingestInteresting and onDecision race to consume the same buffered trace.
func TestConcurrentInterestingAndCoordinatorDecision(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid = "aabbccdd77777777aabbccdd77777777"

	// Wait for the gRPC stream to establish before pre-buffering.
	require.Eventually(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.streams) > 0
	}, 2*time.Second, 10*time.Millisecond, "coordinator stream must connect")

	// Pre-buffer one ok span.
	require.NoError(t, p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeOk)))
	assert.Equal(t, 0, sink.SpanCount(), "ok span should be buffered")

	// Concurrently: error span via ConsumeTraces AND coordinator decision.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(2)
	go func() {
		start.Wait()
		_ = p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeError))
		done.Done()
	}()
	go func() {
		start.Wait()
		fc.sendDecision(tid)
		done.Done()
	}()
	start.Done()
	done.Wait()

	// 1 pre-buffered ok + 1 error = 2 total. Must not be 3 (double-buffered).
	require.Eventually(t, func() bool {
		return sink.SpanCount() >= 2
	}, 2*time.Second, 10*time.Millisecond, "spans must be ingested")
	time.Sleep(50 * time.Millisecond) // let any erroneous second write arrive
	assert.Equal(t, 2, sink.SpanCount(), "buffered span must not be duplicated")
}
```

- [ ] **Step 3: Run new tests with race detector**

```
cd processor/retroactivesampling && go test ./... -run 'TestConcurrent' -v -race -count=10
```

Expected: both tests `PASS` across all 10 runs, no race warnings. (`-count=10` increases the chance of hitting the race.)

- [ ] **Step 4: Run full test suite**

```
cd processor/retroactivesampling && go test ./... -race
```

Expected: all `PASS`, no race warnings.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/processor_test.go
git commit -m "test(processor): concurrent interesting-span and coordinator-decision tests"
```

---

### Task 4: Mark TODO done

**Files:**
- Modify: `TODO.md`

- [ ] **Step 1: Mark the TODO item complete**

In `TODO.md`, change:

```
- [ ] ensure "2 interesting spans in same trace" is handled properly without double writing or any other bugs
```

to:

```
- [x] ensure "2 interesting spans in same trace" is handled properly without double writing or any other bugs
```

- [ ] **Step 2: Commit**

```bash
git add TODO.md
git commit -m "chore: mark double-write TODO as done"
```
