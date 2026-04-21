# Double-Write Fix: Concurrent Interesting Spans in Same Trace

## Problem

`SpanBuffer.Read` and `SpanBuffer.Delete` each acquire the buffer mutex independently. If two goroutines concurrently call `ingestInteresting` for the same trace ID (possible because `processTraces` is not serialized), both can `Read` before either `Delete`s. Both receive the buffered spans, both call `ConsumeTraces`, resulting in duplicate data sent downstream. The same race exists between `ingestInteresting` and `onDecision`.

The processor is the sole gatekeeper — no downstream deduplication.

## Fix

### 1. Buffer (`internal/buffer/buffer.go`)

Add `ReadAndDelete(traceID string) (ptrace.Traces, bool, error)`:
- Acquires the mutex once
- Copies out delta record offsets, deletes `entries[traceID]`, releases lock
- Unmarshals the copied data outside the lock (or inside — data is in the mmap slice, safe to read under lock)
- Returns the merged traces, `true, nil` on success; `false` if no entry existed

Existing `Read` and `Delete` methods remain (used in tests and buffer internals).

### 2. Processor (`processor.go`)

`ingestInteresting`: replace `p.buf.Read` + `p.buf.Delete` (both the normal and error-read paths) with `p.buf.ReadAndDelete`.

`onDecision`: replace `p.buf.Read` + `p.buf.Delete` with `p.buf.ReadAndDelete`.

### 3. Tests (`processor_test.go`)

**`TestConcurrentInterestingSpansSameTrace`**: pre-buffer one ok span for trace X, then concurrently fire two goroutines each delivering an error span for X via `ConsumeTraces`. Sync with `sync.WaitGroup`. Assert the pre-buffered span appears in the sink exactly once (not twice). Run with `-race`.

**`TestConcurrentInterestingAndCoordinatorDecision`**: pre-buffer one span, then concurrently fire an error-span `ConsumeTraces` and a coordinator `sendDecision`. Assert the buffered span appears exactly once.

## Invariant After Fix

For any trace ID, the buffer entry is atomically consumed: the goroutine that wins the `ReadAndDelete` lock delivers the buffered spans; all concurrent losers get `ok=false` and forward only their own current spans. No span is delivered twice, no span is dropped.
