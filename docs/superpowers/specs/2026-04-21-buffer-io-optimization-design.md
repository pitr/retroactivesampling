# Buffer IO Optimization Design

Date: 2026-04-21

## Goal

Eliminate syscall overhead in the hot write path of `SpanBuffer`, remove disk reads from `sweepOneLocked`, and delete the checkpoint machinery that is no longer needed.

## Context

`SpanBuffer` backs a ring buffer with a file on disk to support 10–30 GB of payload at any one time. The processor spends most of its time in syscalls: two `WriteAt` calls per write (header + payload separately), and one `ReadAt` per sweep. The checkpoint (save/load on close/open) is also being removed — there is no requirement to resume the buffer across process restarts.

## Approach: mmap-backed ring

Replace `*os.File` + `WriteAt`/`ReadAt` with a `mmap`-backed `[]byte`. Writes and reads become plain slice copies. The OS manages page cache and async writeback. `madvise(MADV_DONTNEED)` after sweep keeps process RSS proportional to live data rather than total ring capacity.

### Why not alternatives

- **Batch writes (single WriteAt per record)**: reduces syscalls from 2→1 but doesn't eliminate them. sweep still reads from disk.
- **Buffered write path**: complex flush-before-read coupling; sweep still reads from disk.

## Structure changes

Remove from `SpanBuffer`:
- `f *os.File`
- `cpMagic`, `cpVersion` constants
- `saveCP()`, `tryLoadCheckpoint()`, `cpPath()` methods

Add to `SpanBuffer`:
- `data []byte` — the mmap slice
- `lastMadvise int64` — tracks last page boundary past which MADV_DONTNEED was issued

`ringPath()` stays — the file still backs the mmap.

## New/Close

`New`:
1. `os.MkdirAll`
2. Open/create `buffer.ring` with `O_RDWR|O_CREATE`
3. `Truncate(maxBytes)` only if `Stat().Size() < maxBytes` (never shrink an existing ring)
4. `mmap(fd, MAP_SHARED, PROT_READ|PROT_WRITE, length=maxBytes)`
5. `madvise(MADV_SEQUENTIAL)` on full mapping

`Close`:
1. `munmap(b.data)`
2. Close file descriptor
3. No checkpoint write

## Write path

`WriteWithEviction` builds a single `[]byte` of `hdrSize+len(delta)`, fills the header into the first 44 bytes, copies `delta` into the remainder, then does one `copy` into `b.data[offset:]`. No `WriteAt` calls.

`wrapLocked` writes the skip header directly into `b.data[b.wHead:]`.

## Read path

`Read` replaces `f.ReadAt` with a slice copy from `b.data[d.offset+hdrSize:]`.

## sweepOneLocked

Reads the 44-byte header directly from `b.data[b.rHead:]` — a memory read, no syscall. All downstream logic (zero-ID check, traceID extraction, recSize calculation) is unchanged.

After advancing `rHead`, call `madvise(MADV_DONTNEED)` on the swept region aligned to page boundaries. Only issue the call when `rHead` has crossed at least one page boundary past `lastMadvise`, to avoid per-record madvise overhead.

## RSS impact

- VSZ increases by full ring size at startup (virtual address reservation).
- RSS increases relative to today because page-cache pages mapped into the process count toward process RSS, unlike with `ReadAt`/`WriteAt`. With `MADV_DONTNEED` after sweep, RSS tracks live data (between rHead and wHead) rather than total ring capacity.
- This is acceptable; the behavior was confirmed with the operator.

## Tests

Remove: `TestCheckpointRoundtrip`, `TestCheckpointFreshStartOnCrash`, `TestCheckpointMaxBytesMismatch`.

All other tests are unchanged — the public API (`New`, `WriteWithEviction`, `Read`, `Delete`, `Close`) is identical. Existing ring logic tests cover correctness of the mmap path. `madvise` is best-effort and not tested directly.

## Dependencies

`golang.org/x/sys/unix` for `unix.Mmap`, `unix.Munmap`, `unix.Madvise` — already present in `processor/retroactivesampling/go.mod` as an indirect dependency; this change promotes it to direct. Works on both Linux (production) and macOS (development).
