# SpanBuffer: Channel-Based Refactor Design

**Date:** 2026-05-03  
**Scope:** `processor/retroactivesampling/internal/buffer/buffer.go`

## Motivation

The current `SpanBuffer` uses a `sync.Mutex` + `sync.Cond` + `sync.WaitGroup` to coordinate a writer path and a sweeper goroutine. The goal is to replace this with a channel-based actor model: two goroutines with clean `for`/`select` loops, minimal shared mutable state, and channels as the primary coordination mechanism.

## Architecture

Two goroutines. Each owns mutable state exclusively.

**Writer goroutine** (`runWriter`): owns `stage []byte`, `stageN int`, `wHead int64` as goroutine-local variables — never shared. Reads from `writeCh`, auto-flushes via a ticker, exits on `closeCh`. Closes `writerDone` when done.

**Sweeper goroutine** (`runSweeper`): owns `rHead int64`, `interestEntries`, `interestList` as goroutine-local variables. Blocks in a `select` on `wakeup`, `writerDone`. Processes pages sequentially from `rHead`. Signals `pageFreed` after consuming page(s). Takes `interestMu` write lock only to prune expired entries.

**External callers** (`Write`, `Close`, `AddInterest`, `HasInterest`): `Write` is a non-blocking channel send. `Close` signals shutdown and waits on `wg`. `AddInterest` takes `interestMu` write lock and sends a non-blocking wakeup. `HasInterest` takes `interestMu` read lock.

**One shared atomic**: `used atomic.Int64` — writer increments per page flushed, sweeper decrements per page(s) consumed.

## Channel Map

```
writeCh   chan writeReq    cap=pageCount  Write() non-blocking send; ErrFull if full
wakeup    chan struct{}    cap=1          writer sends after flush; AddInterest sends on new interest
pageFreed chan struct{}    cap=1          sweeper sends after consuming page(s)
writerDone chan struct{}   —              writer closes when done; sweeper drains then exits
closeCh   chan struct{}    —              Close() closes this; writer exits loop
```

All three notify channels (`wakeup`, `pageFreed`, and the implicit `writerDone` close) use non-blocking sends or close semantics — a single pending signal is as good as N, since receivers process all available work on wakeup.

`writeCh` capacity = `maxBytes / pageSize` (page count). When the writer goroutine stalls on a full ring buffer (blocking on `pageFreed`), `writeCh` fills and callers get `ErrFull`. Not exact to the byte but scales with ring buffer depth.

## Struct Changes

**Removed:** `writeMu`, `stage`, `stageN`, `scratch`, `wHead`, `rHead`, `used` (field), `closed`, `wakeupPending`, `cond`, `mu` (replaced by `interestMu`)

**Added:**
```go
writeCh   chan writeReq
wakeup    chan struct{}
pageFreed chan struct{}
writerDone chan struct{}
closeCh   chan struct{}
used      atomic.Int64
```

**Unchanged:** `f`, `maxBytes`, `decisionWait`, `onMatch`, `evictObs`, `interestEntries`, `interestList`, `wg`, `interestMu` (renamed from `mu`, scope narrowed to interest state only).

**New unexported type:**
```go
type writeReq struct {
    traceID    pcommon.TraceID
    data       []byte
    insertedAt time.Time
}
```

`New()` sets `writeCh` capacity to `maxBytes/pageSize` and calls `wg.Add(2)` before launching both goroutines.

## Writer Goroutine

```go
func (b *SpanBuffer) runWriter() {
    defer b.wg.Done()
    defer close(b.writerDone)

    stage := make([]byte, pageSize)
    stageN := 0
    wHead := int64(0)

    ticker := time.NewTicker(50 * time.Millisecond)
    defer ticker.Stop()

    flush := func() error {
        clear(stage[stageN:])
        for b.used.Load()+int64(pageSize) > b.maxBytes {
            select {
            case <-b.pageFreed:
            case <-b.closeCh:
                return nil // drop on shutdown; closeCh already signals exit
            }
        }
        if _, err := b.f.WriteAt(stage, wHead); err != nil {
            return err
        }
        wHead = (wHead + int64(pageSize)) % b.maxBytes
        b.used.Add(int64(pageSize))
        stageN = 0
        select { case b.wakeup <- struct{}{}: default: }
        return nil
    }

    handle := func(req writeReq) {
        // small record: accumulate in stage, flush when full
        // large record: flush current stage, then write multi-chunk
        // same logic as current Write(), errors from flush() silently dropped
        // (caller already returned nil; backpressure via writeCh is the signal)
    }

    for {
        select {
        case req := <-b.writeCh:
            handle(req)
        case <-ticker.C:
            if stageN > 0 {
                _ = flush()
            }
        case <-b.closeCh:
            for { // drain remaining writes
                select {
                case req := <-b.writeCh:
                    handle(req)
                default:
                    if stageN > 0 {
                        _ = flush()
                    }
                    return
                }
            }
        }
    }
}
```

Key points:
- `flush()` blocks on `pageFreed` when ring buffer full — stalls writer → fills `writeCh` → callers get `ErrFull`
- On `closeCh` inside `flush()`, drops data and returns; the outer close case then drains and exits
- 50ms ticker ensures partial stages are committed without caller prodding
- `close(writerDone)` is deferred — fires even on early return

## Sweeper Goroutine

```go
func (b *SpanBuffer) runSweeper() {
    defer b.wg.Done()

    scratch := make([]byte, pageSize)
    rHead := int64(0)
    closing := false

    for {
        if b.used.Load() == 0 {
            if closing {
                return
            }
            select {
            case <-b.wakeup:
            case <-b.writerDone:
                closing = true
            }
            continue
        }

        fullyConsumed, chunksConsumed, minRemaining := processPage(scratch, rHead, closing)

        if fullyConsumed {
            rHead = (rHead + int64(chunksConsumed)*int64(pageSize)) % b.maxBytes
            b.used.Add(-int64(chunksConsumed) * int64(pageSize))
            select { case b.pageFreed <- struct{}{}: default: }
            b.pruneInterestLocked() // interestMu write lock, brief
        } else {
            if minRemaining > 0 {
                time.AfterFunc(minRemaining, func() {
                    select { case b.wakeup <- struct{}{}: default: }
                })
            }
            select {
            case <-b.wakeup:
            case <-b.writerDone:
                closing = true
            }
        }
    }
}
```

Key points:
- `closing` flag replaces `b.closed` — set once when `writerDone` fires
- `pressure = b.used.Load() > b.maxBytes*3/4 || closing` — computed per page, no lock
- When `writerDone` fires with `used > 0`, `closing=true` causes all remaining records to be force-evicted or delivered on subsequent iterations
- `processPage` extracts the current sweeper body (single-chunk and multi-chunk paths); takes `interestMu` read lock when checking interest, write lock only in `pruneInterestLocked`
- `writerDone` is a closed channel (broadcast): receiving from it in either wait block returns immediately

## Public API

**`Write`** — non-blocking:
```go
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
    select {
    case b.writeCh <- writeReq{traceID, data, insertedAt}:
        return nil
    default:
        return ErrFull
    }
}
```

**`Close`**:
```go
func (b *SpanBuffer) Close() error {
    close(b.closeCh)
    b.wg.Wait()
    return b.f.Close()
}
```

**`AddInterest`** — write lock + wakeup:
```go
func (b *SpanBuffer) AddInterest(traceID pcommon.TraceID) {
    b.interestMu.Lock()
    // same list update logic (add or move-to-front with renewed addedAt)
    b.interestMu.Unlock()
    select { case b.wakeup <- struct{}{}: default: }
}
```

**`HasInterest`** — read lock, expiry check only (no removal):
```go
func (b *SpanBuffer) HasInterest(traceID pcommon.TraceID) bool {
    b.interestMu.RLock()
    defer b.interestMu.RUnlock()
    el, ok := b.interestEntries[traceID]
    if !ok {
        return false
    }
    return time.Since(el.Value.(*interestEntry).addedAt) < b.decisionWait
}
```

Stale entries linger in the map until `pruneInterestLocked` runs in the sweeper — correctness is maintained since `HasInterest` returns `false` for expired entries regardless.

**`Flush` is removed.** The processor's `processTraces` drops its `buf.Flush()` call. The 50ms ticker in the writer goroutine provides timely stage commits without caller involvement.

## ErrFull Semantics Change

Previously `ErrFull` reflected exact ring buffer occupancy. Now it reflects `writeCh` being full (approximate). `writeCh` capacity = page count, so channel-full tracks ring-buffer-full closely but not exactly.

## Test Adaptations

`TestWrite_full` and `TestWrite_largeRecord_full` pin exact ring-buffer-full semantics and must be rewritten. New approach: write in a loop until `ErrFull` is returned, assert it occurs before an upper bound (e.g. `10 * pageCount` iterations). Exact count no longer matters; what matters is that backpressure eventually produces `ErrFull`.

Tests that called `buf.Flush()` before `Close()` drop the `Flush()` call — `Close()` is already the sync point.

All other tests are unaffected.
