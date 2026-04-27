# Buffer write-path performance: write-coalescing staging buffer

## Context

`processor/retroactivesampling/internal/buffer/buffer.go` is a file-backed ring
buffer for span data. Writes are frequent (every trace flowing through the
processor that isn't already known interesting). Reads are rare (1–5% of traces
get sampled and delivered).

Current `BenchmarkWrite` (steady-state under eviction pressure, Apple M1 Pro):

```
BenchmarkWrite-10   416064   5119 ns/op   515 B/op   6 allocs/op
```

Per-write cost is dominated by:

1. `unix.Pwritev` syscall on every Write (~1.5µs).
2. Sweeper `ReadAt` syscall on every eviction (~1.5µs).
3. Writer↔sweeper context switch via `sync.Cond` and `wakeC` (~1µs).
4. `sync.Mutex` held across all of the above, serialising the rest of the work.

Goal: get steady-state Write to ≤500 ns/op (10×) with 0 allocs/op on the hot
path, without raising RAM beyond a small fixed overhead. mmap is off the table:
prior investigation (commit `9d64a3e`) confirmed great perf but unacceptable RSS
growth — using disk to keep memory low is the whole point of the buffer.

## Approach

Add a small in-memory **staging buffer** that absorbs writes between disk
flushes. Move eviction into the writer's flush path so the writer↔sweeper
round-trip disappears from the hot path. Keep the sweeper goroutine only for
delivery of interesting records.

### Why this works for the workload

- Writes vastly outnumber reads. Amortising syscalls across the write stream
  is high leverage.
- Records are small (~100B–10KB). A 4 KiB stage holds ~30 typical records,
  giving ~30× syscall amortisation.
- Eviction is purely uninteresting most of the time (1–5% sampling rate), so
  the writer can drop records inline without needing the user-callback
  goroutine.

### Memory cost

One fixed `stageCap` allocation (default 4 KiB) plus a small scratch buffer for
batched header reads (default 4 KiB). Total fixed overhead: ~8 KiB per
`SpanBuffer`. Independent of throughput. Within the "low RAM" intent.

## Components

### State changes on `SpanBuffer`

```go
type SpanBuffer struct {
    // unchanged: maxBytes, f, fd, wHead, rHead, decisionWait, interestEntries,
    // interestList, onMatch, evictObs, mu, wakeC, closeC, sweeperDone, closed

    used     int64   // CHANGED semantics: counts disk-live bytes + stage bytes

    // NEW
    stage     []byte // len=pending bytes; cap=stageCap
    stageCap  int    // configurable, default 4096; must be >= maxRecordSize
    scratch   []byte // for batched header reads during eviction; cap=evictScanCap
    flushDone *sync.Cond
    flushing  bool   // true while one writer owns the flush slot
}
```

`b.cond` is replaced (or supplemented) by `flushDone`, which is the only
condition variable other writers wait on.

### New configuration

`New` gains a `stageCap int` parameter (or option). Default 4096. Validated
`stageCap >= 2*hdrSize` and `stageCap <= maxBytes/2` (latter so wrap math
remains simple).

## Data flow

### Write hot path

```
lock mu
recSize = hdrSize + len(data)
if recSize > stageCap:               return ErrRecordTooLarge
if len(stage) + recSize > stageCap:  flushLocked()    // slow path
encode header into stage[len:len+hdrSize]
copy data into stage[len+hdrSize:]
stage = stage[:len+recSize]
used += recSize
unlock mu
return nil
```

No syscall. No sweeper signal. No `cond.Wait` in the common case.

### flushLocked (slow path, ~1 in N writes)

```
// 1. Reserve flush slot.
for flushing && !closed:
    flushDone.Wait()
if closed: return ErrClosed
flushing = true

// 2. Wrap handling.
if wHead + len(stage) > maxBytes:
    if maxBytes - wHead >= hdrSize:
        emit a skip-record header at end of stage prefix
        // OR: pre-write padding header here in a tiny syscall (decided at
        // implementation time; padding-in-stage is preferred)
    advance bookkeeping so the post-flush wHead resets to 0

// 3. Inline batch eviction.
evictBatchedLocked(len(stage))

// 4. Drop lock for the syscall.
buf := stage; off := wHead
unlock mu
n, err := unix.Pwrite(fd, buf, off)
lock mu

// 5. Commit & wake other writers.
if err != nil { flushing = false; flushDone.Broadcast(); return err }
wHead = (wHead + int64(n)) % maxBytes  // wraps cleanly given step 2
stage = stage[:0]
flushing = false
flushDone.Broadcast()
return nil
```

### evictBatchedLocked(need)

Frees `need` bytes at the disk wHead by advancing rHead.

```
for (used - len(stage)) + need > maxBytes:
    readLen := min(maxBytes - rHead, evictScanCap)
    drop mu
    n, err := f.ReadAt(scratch[:readLen], rHead)
    lock mu
    if err != nil && n == 0: return err
    parse headers walking forward through scratch[:n]:
        if traceID is empty (skip-record):
            rHead = 0; continue (next iteration uses fresh ReadAt)
        if hasInterestLocked(traceID):
            // Option A: hand off to sweeper, wait, retry.
            wake sweeper via wakeC
            for hasInterestLocked(traceID) && !closed: flushDone.Wait()
            // sweeper advances rHead past the delivered record; loop continues.
            break parsing; restart outer loop
        rHead += recSize
        used  -= recSize
        evictObs(time.Since(insertedAt))
        if (used - len(stage)) + need <= maxBytes: break
```

The sweeper, when it advances rHead, must call `flushDone.Broadcast()` so a
writer waiting in eviction wakes up.

### Sweeper (delivery-only)

```
for !closed:
    select { case <-wakeC: case <-closeC: return }
    lock mu; r := rHead; w := wHead; unlock mu
    walk records [r, w):
        read header (ReadAt; could be batched the same way as eviction)
        if !hasInterest && age < decisionWait: skip ahead in this scan,
                  but do NOT advance rHead past it (eviction owns rHead)
        if hasInterest:
            read payload, call onMatch
            advance rHead past this record under lock
            broadcast flushDone (a writer may be waiting)
```

Key change: sweeper no longer evicts. It only advances rHead for records it
*delivers*. Eviction (advancing rHead past undelivered records) belongs to the
writer flush path. This removes the dual-writer-of-rHead hazard by making
ownership clear: writer drops uninteresting records, sweeper drops delivered
ones.

Coordination: both update rHead under `mu`. Both broadcast `flushDone` when
they advance it.

### HasInterest / AddInterest

Unchanged. `AddInterest` still kicks `wakeC` so the sweeper picks up newly
interesting records that are already on disk.

### Close

```
lock mu
closed = true
flushDone.Broadcast()           // unblock any waiting writer
flush stage if non-empty        // so sweeper can deliver pending records
unlock mu
close(closeC)
<-sweeperDone
return f.Close()
```

## Invariants

1. `used` = disk-live bytes + `len(stage)`.
2. `stage` content is append-order. Headers in `stage` carry the original
   `insertedAt`; age-based decisions remain correct after flush.
3. `wHead` advances only on successful flush. `rHead` advances on writer
   eviction (uninteresting drop) or sweeper delivery.
4. `stageCap >= maxRecordSize` so any single accepted record fits after flush.
5. Only one flush in flight at a time (`flushing` flag).
6. Mutex is never held across a syscall (Pwrite, ReadAt are done with `mu`
   released).

## Error handling

| Condition                              | Behaviour                              |
| -------------------------------------- | -------------------------------------- |
| `recSize > stageCap`                   | Return error, no state change.         |
| `recSize > maxBytes`                   | Return error (existing).               |
| `Pwrite` short write or error          | Propagate error, reset `flushing`, broadcast `flushDone`, do not advance `wHead`, do not reset `stage`. Stage contents are retained for retry on next Write. The current Write call (which triggered the flush) returns the error before its record is appended — so its record is dropped at the call site. |
| `ReadAt` error during eviction         | Propagate error from flush. Records already evicted in this batch stay evicted (rHead has advanced). Stage is untouched. Same Write-call behaviour as Pwrite error. |
| `Close` while writer waits in flush    | Writer returns ErrClosed.              |
| Crash before flush                     | Up to `stageCap` bytes of recent records lost. Documented.              |

Stage is RAM-only. The buffer is for retroactive sampling, not durable storage;
this trade-off is consistent with current behaviour (sweeper already drops
records).

## Concurrency

- Few concurrent writers (2–16 from OTel worker pool).
- Hot path: one mutex acquire/release per Write, no syscall, no condvar.
- Slow path: one writer holds the flush slot via `flushing` flag; others wait
  on `flushDone`. Pwrite runs with mutex released (single-writer-per-flush
  ensures no overlapping writes to disk).
- Sweeper coordinates via `mu` + `flushDone` for rHead advances.

## Observability

- `LiveBytes` includes stage bytes (records committed to be delivered or
  dropped).
- `OrphanedBytes` semantics unchanged.
- Consider adding a `StageBytes` getter for tests/diagnostics.

## Testing

### Existing tests must keep passing

Wrap, eviction, interest, close, concurrent writers, age-based delivery. Tests
that observed disk state immediately after `Write` may need to flush first;
introduce an unexported `flushForTest()` helper if needed.

### New unit tests

- Stage flush triggers exactly when next record would overflow.
- Record larger than `stageCap` errors without mutating state.
- Multiple concurrent writers: total bytes accepted == total bytes flushed,
  no double-flush, no lost records.
- AddInterest on a record currently in stage: after flush, sweeper delivers it.
- Interesting record encountered during writer eviction: writer waits, sweeper
  delivers, writer resumes and completes its flush.
- Close with non-empty stage: pending records are flushed and any interesting
  ones delivered before Close returns.
- Wrap during flush: skip-record padding correct; subsequent reads see records
  in correct order.

### Benchmarks

- `BenchmarkWrite` (existing): target ≤500 ns/op, 0 allocs/op.
- New `BenchmarkWriteConcurrent` (8 writers): verify mutex contention isn't the
  new bottleneck.
- New `BenchmarkWriteWithDelivery` (5% interesting): verify the rare path
  doesn't regress significantly.

## Out of scope

- Returning to mmap (RAM cost rejected previously).
- Lock-free / per-CPU staging (concurrency level doesn't justify complexity).
- io_uring / kqueue async submit (cross-platform cost; staging gets us 10×).
- Changing the on-disk record format.
- Changing the public API beyond adding `stageCap` to `New`.

## Risks

- **Latency for interesting records sitting in stage**: an AddInterest fired
  just after a Write needs the stage to flush before sweeper sees the record.
  Worst case: stage flush triggered only by next overflow. Mitigation if this
  matters in production: AddInterest forces a flush (one extra syscall on the
  rare path).
- **Eviction stalls under interest bursts**: writer in eviction handing off to
  sweeper repeatedly could increase tail latency on flush. Acceptable given 1–5%
  interest rate; revisit if observed.
- **Mutex hold time**: hot path is short (memcpy + arithmetic). Should not
  meaningfully degrade vs current. Validated by `BenchmarkWriteConcurrent`.
