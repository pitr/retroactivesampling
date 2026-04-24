# Buffer B1+C1: Binary TraceID Key + Orphaned Bytes Metric

**Date:** 2026-04-24  
**Status:** Approved

## Problem

Two bugs in `SpanBuffer`:

1. **GC amplification / CPU runaway (B1):** `sweepOneLocked` calls `string(bytes.TrimRight(hdr[:32], "\x00"))` on every sweep — one heap allocation per record evicted. At high eviction rates, GC can't keep up, goroutines pile up on the buffer mutex, CPU spikes to 90–140%. Confirmed via pprof profiles (Apr 21–22).

2. **Orphaned bytes reduce retention (C1):** `ReadAndDelete` removes traceID from the entries map but bytes remain in `used`. At 50% sampling rate with 1GB buffer / 3MB/s ingress, effective retention drops from ~333s to ~167s. No metric exists to observe this.

## Design

### B1 — Binary TraceID in ring header and entries map

**Ring header format change:**

| Field      | Current      | New          |
|------------|-------------|--------------|
| traceID    | 32 hex bytes | 16 binary bytes |
| insertedAt | 8 bytes      | 8 bytes      |
| dataLen    | 4 bytes      | 4 bytes      |
| **total**  | **44 bytes** | **28 bytes** |

`hdrSize` changes from 44 to 28. Saves 16 bytes per record on disk.

**Entries map:** `map[string][]deltaRecord` → `map[[16]byte][]deltaRecord`

Sweep hot path becomes allocation-free:
```go
var key [16]byte
copy(key[:], hdr[:16])
```

**Buffer public API** changes to accept/return `[16]byte`:
```go
func (b *SpanBuffer) WriteWithEviction(traceID [16]byte, spans ptrace.Traces, insertedAt time.Time) error
func (b *SpanBuffer) ReadAndDelete(traceID [16]byte) (ptrace.Traces, bool, error)
```

**`groupByTrace` in `split.go`:** Already uses `map[pcommon.TraceID]ptrace.Traces` internally. Remove the final conversion loop and change the return type to `map[pcommon.TraceID]ptrace.Traces`. `pcommon.TraceID` is `[16]byte`.

**`processor.go` call sites:** Change loop variable type from `string` to `pcommon.TraceID`. Pass directly to buffer methods (binary compatible with `[16]byte`). Coordinator notify and interest cache calls remain `string` — use `tid.String()` there.

**Breaking change:** Existing ring files have the old 44-byte header format. On deploy, the ring file must be deleted (or will be misread). The ring is lossy by design; data loss on upgrade is acceptable and expected.

### C1 — Orphaned bytes observable gauge

Add `liveBytes int64` to `SpanBuffer`. Invariant: `liveBytes ≤ used`, `orphanedBytes = used - liveBytes ≥ 0`.

| Event | `used` | `liveBytes` |
|---|---|---|
| `WriteWithEviction` writes record | `+recSize` | `+recSize` |
| `sweepOneLocked` evicts live record (still in entries) | `-recSize` | `-recSize` |
| `sweepOneLocked` sweeps orphaned/pad record | `-recSize` | unchanged |
| `ReadAndDelete` removes traceID | unchanged | `-(hdrSize + d.size)` per delta |

Expose:
- `LiveBytes() int64` — bytes still indexed in `entries`
- `OrphanedBytes() int64` — `used - liveBytes` (bytes in ring but no longer indexed; pending sweep or pad)

**New metrics in `metadata.yaml`:**
```yaml
retroactive_sampling_buffer_live_bytes:
  description: Bytes in the ring buffer still reachable by traceID
  unit: By
  gauge:
    value_type: int

retroactive_sampling_buffer_orphaned_bytes:
  description: Bytes in ring buffer used accounting but no longer indexed (pending sweep)
  unit: By
  gauge:
    value_type: int
```

Register as async observable gauges in `generated_telemetry.go` (via mdatagen). Wire the callback in `processor.go` using `buf.LiveBytes()` and `buf.OrphanedBytes()` accessors.

## Files Changed

| File | Change |
|------|--------|
| `internal/buffer/buffer.go` | `hdrSize` 44→28; map key `[16]byte`; API `[16]byte`; `liveBytes` field + accessors |
| `split.go` | Return `map[pcommon.TraceID]ptrace.Traces`; remove string conversion loop |
| `processor.go` | Update loop type; update buffer call sites; register gauge callbacks |
| `metadata.yaml` | Add two gauge metrics |
| `internal/metadata/generated_telemetry.go` | Regenerate (mdatagen) or hand-edit to add gauge fields |

## Testing

- Update existing buffer unit tests to use `[16]byte` traceIDs
- Add test: `ReadAndDelete` of a known record → `OrphanedBytes()` increases, `LiveBytes()` decreases
- Add test: sweep of orphaned record → `OrphanedBytes()` decreases (implicitly via `used`)
- Existing processor tests pass with `pcommon.TraceID` loop variable

## Deployment Note

Delete ring buffer file(s) before rolling out this change. No data migration path.
