# Buffer Redesign: Ring Buffer with Delta Writes

## Context

Production: 300 MB/s per collector, 15 GB buffer.

Current design (`buffer/buffer.go`): file-per-trace, one `.bin` file per traceID in a flat directory.

Problems at scale: ~11.5 M files, 46 GB disk for 15 GB data (4 KB block waste), 736 MB RAM for index, O(n) eviction, minutes of startup time.

## Goal

Replace with a single pre-allocated ring buffer file. Disk usage = exactly `maxBytes`, always. Near-instant startup on normal restart.

---

## Design

### Ring file (`buffer.ring`)

Pre-allocated to exactly `maxBytes` bytes via `os.Truncate`. Never grows. Never shrinks.

Records are written sequentially. Each record:

```
[traceID: 32 bytes][insertedAt: 8 bytes][dataLen: 4 bytes][data: dataLen bytes]
```

Total header = 44 bytes. `traceID` is stored as the raw string bytes, zero-padded to 32 bytes (OTel trace IDs are 32-char hex; tests must use proper-length IDs).

**Skip record**: when a record would not fit in the remaining bytes at the end of the file, write a skip record at `writeHead` and wrap `writeHead` to 0:

```
[zeros: 32 bytes][0: 8 bytes][remaining: 4 bytes]
```

`remaining` = `maxBytes - writeHead - 44`. When `readHead` encounters a skip record (traceID == all zeros), it sets `readHead = 0`.

If `maxBytes - writeHead < 44` (not enough room for even a skip header): set `used += maxBytes - writeHead`, set `writeHead = 0`, write no skip record. `readHead` wraps automatically: after any advance, `if readHead >= maxBytes { readHead = 0 }`.

### In-memory structures

```go
type deltaRecord struct {
    offset int64
    size   int32
}

type SpanBuffer struct {
    f         *os.File
    maxBytes  int64
    writeHead int64
    readHead  int64
    used      int64                      // (writeHead - readHead + maxBytes) % maxBytes
    entries   map[string][]deltaRecord
    mu        sync.Mutex
}
```

No min-heap. No `onEvict` callback. Eviction is implicit: `readHead` advances through old records as new ones are written.

### WriteWithEviction

1. Marshal spans → `delta` bytes.
2. `recordSize = 44 + len(delta)`.
3. If `recordSize > maxBytes - 44`: return error (record larger than ring; impossible at production scale with 1.3 KB avg trace, but guard it).
4. If `writeHead + recordSize > maxBytes`: write skip record at `writeHead`, advance `used` by `(maxBytes - writeHead)`, set `writeHead = 0`.
5. While `used + recordSize > maxBytes`: sweep one record from `readHead`:
   - Read 44-byte header at `readHead`.
   - If traceID == zeros (skip record): `readHead = 0`, `used -= header.remaining + 44`.
   - Else: remove `{readHead, header.dataLen}` from `entries[traceID]`; if slice empty, delete key.
   - `readHead += 44 + header.dataLen`; `used -= 44 + header.dataLen`.
6. Write record at `writeHead` (pwrite).
7. Append `deltaRecord{writeHead, int32(len(delta))}` to `entries[traceID]`.
8. `writeHead += recordSize`; `used += recordSize`.

Step 5 removes only the swept delta from the trace's list. The trace stays alive with its remaining deltas. It disappears from `entries` only when its last delta is swept.

### Read

1. Look up `entries[traceID]`. If absent: return `ptrace.Traces{}, false, nil`.
2. For each `deltaRecord{offset, size}`: `pread(f, buf, offset+44, size)` — skip the 44-byte header, read only span data.
3. Unmarshal each chunk, merge via `MoveAndAppendTo`.
4. Return merged traces.

Read is O(k) syscalls for k deltas. Reads are rare (only when a trace is sampled), so this is acceptable.

### Delete

Remove `traceID` from `entries`. The ring records remain on disk; they are swept naturally when `readHead` advances past them (traceID not in `entries` → just advance, no side effects).

### Close

Write checkpoint (see below), then close file.

---

## Checkpoint (`buffer.checkpoint`)

Written on clean shutdown, loaded on startup for near-instant index reconstruction.

**Format:**

```
[magic: 4 bytes = 0x52494E47 ("RING")][version: 4 bytes = 1]
[maxBytes: 8 bytes][writeHead: 8 bytes][readHead: 8 bytes]
[entryCount: 4 bytes]
For each entry:
  [traceIDLen: 1 byte][traceID: traceIDLen bytes]
  [deltaCount: 4 bytes]
  For each delta:
    [offset: 8 bytes][size: 4 bytes]
```

**Startup sequence:**

1. If `buffer.checkpoint` exists:
   - Read and validate magic + version.
   - If `maxBytes` in checkpoint ≠ configured `maxBytes`: treat as fresh start (maxBytes changed).
   - Load `writeHead`, `readHead`, `entries` → instant.
   - Delete checkpoint file (crash before next checkpoint = fresh start next time).
   - Open ring file (must already exist at correct size).
2. Else (no checkpoint = fresh start or post-crash):
   - Create/truncate ring file, `os.Truncate(path, maxBytes)`.
   - `entries = map[string][]deltaRecord{}`, `writeHead = readHead = used = 0`.

Crash safety: if the process dies after deleting the checkpoint but before writing a new one, the next startup finds no checkpoint and begins fresh — empty buffer. Acceptable per design requirements (simplicity > crash safety).

---

## Interface changes

**`maxBytes = 0` (unlimited mode) is no longer supported.** A ring buffer must be pre-allocated. `New` returns an error if `maxBytes <= 0`. Tests that currently use `maxBytes = 0` must set a finite value (e.g., compute the exact size of the records they intend to write, as the eviction tests already do).

Remove `onEvict func(string, time.Time)` from `New()`. The processor uses `onEvict` only for:
- Deleting from the interest cache (stale entries are harmless; cache has its own eviction).
- Recording the retention histogram metric (nice-to-have, not critical).

Updated signature:

```go
func New(dir string, maxBytes int64) (*SpanBuffer, error)
```

`New` creates `dir` if needed, then follows the startup sequence above. The ring file lives at `filepath.Join(dir, "buffer.ring")` and the checkpoint at `filepath.Join(dir, "buffer.checkpoint")`.

Preserved: `WriteWithEviction`, `Read`, `Delete`, `Close`.

---

## Space accounting

- `used` = `(writeHead - readHead + maxBytes) % maxBytes` (updated incrementally).
- Invariant: `used ≤ maxBytes - 44` (always room for at least one skip record header).
- Disk: ring file = exactly `maxBytes`. Checkpoint file ≈ 200–400 MB at 11.5 M traces; loaded sequentially on startup (~0.5–1 s at typical disk speeds), then deleted.

Deleted traces occupy ring space until swept. At steady state (traces kept for ~50 s at 300 MB/s), deleted-but-not-swept bytes are a small fraction of `maxBytes`.

---

## Properties vs. current design

| | Current | Ring buffer |
|---|---|---|
| Disk usage | 3× live data (4 KB blocks) | exactly `maxBytes` |
| Files / inodes | 11.5 M | 2 (ring + checkpoint) |
| RAM (index) | ~736 MB | ~736 MB (same entry count) |
| Eviction | O(n) scan | implicit O(1) amortized |
| Startup (normal) | minutes | < 1 s (load checkpoint) |
| Startup (post-crash) | minutes | instant (empty buffer) |
| Syscalls | 75 K/s (random r+w) | sequential writes + rare preads |

---

## Files touched

- `processor/retroactivesampling/internal/buffer/buffer.go` — full rewrite
- `processor/retroactivesampling/internal/buffer/buffer_test.go` — update tests (proper 32-char traceIDs, remove onEvict tests)
- `processor/retroactivesampling/processor.go` — remove `onEvict` argument from `buffer.New`
