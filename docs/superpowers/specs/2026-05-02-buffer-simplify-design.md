# SpanBuffer: remove configurable chunk size, support large records

**Date:** 2026-05-02
**Scope:** `processor/retroactivesampling/internal/buffer/buffer.go` and its tests; `config.go`; `processor.go`

## Goals

1. Remove `chunkSize` as a user-configurable parameter.
2. Support traces (records) of unbounded size — currently a record must fit in one chunk.
3. Simplify by dropping out-of-order (priority) delivery.

## Non-goals

- Changing the file-backed ring buffer structure.
- Changing the interest-set API (`AddInterest`, `HasInterest`).
- Supporting process-restart recovery (not currently supported, still not in scope).

## API changes

### `buffer.New`

Remove the `chunkSize int` parameter:

```go
// before
func New(file string, maxBytes int64, chunkSize int, decisionWait time.Duration, ...) (*SpanBuffer, error)

// after
func New(file string, maxBytes int64, decisionWait time.Duration, ...) (*SpanBuffer, error)
```

### `Config`

Remove `ChunkSize int` / `chunk_size` field from `processor/retroactivesampling/config.go`.

## Internal constants

```go
var pageSize = os.Getpagesize()  // package-level, computed once
```

Replaces all uses of `chunkSize` / `b.chunkSize` in the implementation. `maxBytes` is rounded down to a `pageSize` multiple; must be ≥ `2*pageSize` after rounding.

## Record format

Unchanged: `hdrSize = 28` (16B traceID + 8B insertedAt + 4B dataLen). `dataLen` always encodes the **total** payload length, including in the first chunk of a multi-chunk record.

## Write path

**Small records** (`hdrSize + len(data) <= pageSize`): unchanged — stage accumulates records, flushes when full.

**Large records** (`hdrSize + len(data) > pageSize`):

1. If `stageN > 0`, flush stage (large records must start at offset 0 of their chunk).
2. Write header at `stage[0:]`, fill `stage[hdrSize:]` with the first `pageSize-hdrSize` bytes of data → flush.
3. For each remaining full page: fill `stage[0:]` with raw continuation bytes → flush.
4. Final partial page: write remaining bytes, zero-pad → flush.

**Continuation chunks** carry no header — their presence is inferred by the sweeper from `dataLen > pageSize-hdrSize` in the first chunk's header. The invariant "large records start at pos=0" is enforced by the flush-first rule.

**`ErrFull` for large records:** computed before writing any chunk. If `used + numChunks*pageSize > maxBytes`, return `ErrFull` without writing.

## Sweeper

**Removed:** `tryDeliverPriority`, `priorityDelivery map[pcommon.TraceID]struct{}`. `AddInterest` still signals the sweeper cond but only the FIFO path runs.

**Multi-chunk detection** (at `pos == 0` only, since large records always start there):

```
numChunks = 1 + ceil((dataLen - (pageSize-hdrSize)) / pageSize)
```

- If `used < numChunks*pageSize`: record partially flushed — break, wait.
- If interesting or expired: read continuation chunks, assemble full payload, deliver or evict, tombstone first chunk only.
- `rHead` advances by `numChunks * pageSize`; `used` decreases by `numChunks * pageSize`.
- No further `pos` iteration for that chunk (it is fully consumed by the one record).

Single-chunk records: existing `pos`-based iteration unchanged.

**Tombstoning multi-chunk records:** write `tombstoneID` into bytes `[0:16]` of the first chunk. Sweeper seeing tombstoneID at pos=0 with `dataLen > pageSize-hdrSize` skips all N chunks without reading continuation data.

## Testing

**Updated:**
- All `buffer.New` call sites drop the `chunkSize` arg.
- Tests using `cs := 128` switch to `ps := os.Getpagesize()` with recalculated record counts.
- `TestNew_validation/chunkSize too small` removed (no longer applicable).
- `TestSweeper_crossChunkDelivery` removed (priority delivery gone).

**New tests:**
- `TestWrite_largeRecord` — 3-page payload, verify delivery after `AddInterest`.
- `TestSweeper_largeRecordEviction` — expired large record, verify `evictObs` called once.
- `TestWrite_largeRecord_full` — ring has exactly 2 free chunks (< 3 needed), 3-chunk write returns `ErrFull`.
