# Buffer write path: pwritev optimization

**Date:** 2026-04-27  
**Status:** Approved  
**Scope:** `processor/retroactivesampling/internal/buffer/buffer.go`

## Problem

The `SpanBuffer.Write` method issues two separate `pwrite(2)` syscalls per record: one for the 28-byte header and one for the payload. On the wrap boundary a third syscall writes the pad record. All three happen while holding `mu`, serializing concurrent callers.

Profiling confirms `Write` is the hot path. Payloads are 1–10 KB. Reducing syscall count directly reduces lock-hold time and improves both latency and throughput.

## Solution

Replace the two `WriteAt` calls (header + payload) with a single `unix.Pwritev` scatter-gather I/O syscall. This writes both iovecs atomically in one kernel transition, halving the syscall count in the normal path and reducing it from 3 to 2 on the wrap boundary.

The pad record written at wrap is unchanged — it is already a single syscall.

## Implementation

### `SpanBuffer` struct

Add one field:

```go
fd int  // raw file descriptor for unix.Pwritev; set once in New, never changes
```

### `New()`

After the `SpanBuffer` is constructed and before `runSweeper` is started:

```go
b.fd = int(f.Fd())
```

`os.File.Fd()` puts the file into blocking mode. For a regular disk file this has no behavioral effect: disk files never use the Go runtime's non-blocking poller. Subsequent `f.ReadAt` calls in the sweeper continue to work correctly.

### `Write()`

Replace:

```go
if _, err := b.f.WriteAt(hdr[:], int64(b.wHead)); err != nil {
    return err
}
if _, err := b.f.WriteAt(data, int64(b.wHead+hdrSize)); err != nil {
    return err
}
```

With:

```go
if _, err := unix.Pwritev(b.fd, [][]byte{hdr[:], data}, int64(b.wHead)); err != nil {
    return err
}
```

### Dependencies

`golang.org/x/sys/unix` is already present as an indirect dependency (`v0.43.0`). Adding a direct import promotes it to a direct dependency in `go.mod`. Run `go mod tidy` after the change.

### Platform

`unix.Pwritev` is available on Linux and macOS. Windows is not supported (documented in README).

## Correctness

- `pwritev` writes both iovecs to the file at `offset` in a single kernel operation. The kernel does not interleave this with other `pwrite`/`pwritev` calls at the same offset from other processes/goroutines. Since only one goroutine holds `mu` at a time and `wHead` is only advanced under `mu`, there is no concurrent writer at the same offset.
- Short writes on regular local files cannot occur except on disk-full conditions, which are already surfaced as errors. Error handling is identical to the current code.
- The sweeper uses `b.f.ReadAt`, which is unaffected.

## Syscall count

| Path | Before | After |
|---|---|---|
| Normal write | 2 | 1 |
| Wrap boundary write | 3 | 2 |

## Implementation note: iovec slice allocation

The `[][]byte{hdr[:], data}` literal passed to `Pwritev` may heap-allocate (2 × `[]byte` headers = 48 bytes) if the compiler cannot prove it doesn't escape through the `unix.Pwritev` call. Verify with `go build -gcflags="-m"`. If it escapes, replace with a local `[2][]byte` array:

```go
iovs := [2][]byte{hdr[:], data}
unix.Pwritev(b.fd, iovs[:], int64(b.wHead))
```

The fixed-size array is stack-allocated. `iovs[:]` pointing into it does not escape since `Pwritev` issues a synchronous syscall and the kernel copies the iovec structure before returning.

## Testing

- Existing unit tests in `buffer_test.go` cover correctness; no new tests required.
- `BenchmarkWrite` in `buffer_bench_test.go` measures before/after throughput.
- Run `go test ./...` and `go test -bench=. -benchmem` in the buffer package.
