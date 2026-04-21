# Buffer IO Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `WriteAt`/`ReadAt` syscalls in `SpanBuffer` with an mmap-backed ring, eliminating syscall overhead on the hot write path and disk reads in sweep, while removing the now-unneeded checkpoint machinery.

**Architecture:** The ring file is memory-mapped at startup (`unix.Mmap`, `MAP_SHARED`). All reads and writes become slice copies into `b.data`. `sweepOneLocked` reads headers directly from the mmap slice. `madvise(MADV_DONTNEED)` is called page-aligned after each sweep to keep process RSS proportional to live data. Checkpoint code (save/load/magic/version) is deleted entirely.

**Tech Stack:** `golang.org/x/sys/unix` (already in go.mod as indirect; promoted to direct by this change). No other new dependencies.

---

## File Map

| File | Change |
|------|--------|
| `processor/retroactivesampling/internal/buffer/buffer.go` | Full rewrite — mmap, no checkpoint |
| `processor/retroactivesampling/internal/buffer/buffer_test.go` | Remove 3 checkpoint tests |

---

## Task 1: Remove checkpoint tests

The checkpoint tests will be invalid after the rewrite. Remove them first so the remaining tests serve as a passing baseline throughout.

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 1: Delete the three checkpoint test functions**

  Remove these three functions entirely from `buffer_test.go` (lines 212–270 in the current file):

  ```
  func TestCheckpointRoundtrip(t *testing.T) { ... }
  func TestCheckpointFreshStartOnCrash(t *testing.T) { ... }
  func TestCheckpointMaxBytesMismatch(t *testing.T) { ... }
  ```

  After deletion, the file should end at the closing brace of `TestWrapWithSkipRecord`.

- [ ] **Step 2: Run tests — confirm 10 tests pass, 0 fail**

  ```bash
  cd processor/retroactivesampling && go test ./internal/buffer/... -v
  ```

  Expected output (all PASS, no FAIL):
  ```
  --- PASS: TestNewRejectsZeroMaxBytes
  --- PASS: TestWriteAndRead
  --- PASS: TestWriteAppendsSpans
  --- PASS: TestReadMissingTrace
  --- PASS: TestEviction
  --- PASS: TestPartialDeltaEviction
  --- PASS: TestWrap
  --- PASS: TestDelete
  --- PASS: TestDeleteNonExistent
  --- PASS: TestDeleteOrphanSweptOnNextWrite
  --- PASS: TestWrapWithSkipRecord
  PASS
  ```

- [ ] **Step 3: Commit**

  ```bash
  git add processor/retroactivesampling/internal/buffer/buffer_test.go
  git commit -m "test(buffer): remove checkpoint tests"
  ```

---

## Task 2: Rewrite buffer.go with mmap

Replace the entire file. The public API (`New`, `WriteWithEviction`, `Read`, `Delete`, `Close`) is unchanged so all 10 remaining tests must pass after this step.

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`
- Modify: `processor/retroactivesampling/go.mod` (auto-updated by go mod tidy)

- [ ] **Step 1: Replace buffer.go with the mmap implementation**

  Overwrite `processor/retroactivesampling/internal/buffer/buffer.go` with:

  ```go
  package buffer

  import (
  	"bytes"
  	"encoding/binary"
  	"fmt"
  	"os"
  	"path/filepath"
  	"sync"
  	"time"

  	"golang.org/x/sys/unix"

  	"go.opentelemetry.io/collector/pdata/ptrace"
  )

  const hdrSize = 44 // traceID(32) + insertedAt(8) + dataLen(4)

  var (
  	zeroID      [32]byte
  	mmapPageSize = int64(os.Getpagesize())
  )

  type deltaRecord struct {
  	offset int64
  	size   int32
  }

  type SpanBuffer struct {
  	dir         string
  	maxBytes    int64
  	f           *os.File
  	data        []byte
  	wHead       int64
  	rHead       int64
  	used        int64
  	lastMadvise int64
  	entries     map[string][]deltaRecord
  	mu          sync.Mutex
  }

  func New(dir string, maxBytes int64) (*SpanBuffer, error) {
  	if maxBytes <= 0 {
  		return nil, fmt.Errorf("maxBytes must be positive, got %d", maxBytes)
  	}
  	if err := os.MkdirAll(dir, 0700); err != nil {
  		return nil, err
  	}
  	f, err := os.OpenFile(filepath.Join(dir, "buffer.ring"), os.O_RDWR|os.O_CREATE, 0600)
  	if err != nil {
  		return nil, err
  	}
  	info, err := f.Stat()
  	if err != nil {
  		f.Close()
  		return nil, err
  	}
  	if info.Size() < maxBytes {
  		if err := f.Truncate(maxBytes); err != nil {
  			f.Close()
  			return nil, err
  		}
  	}
  	data, err := unix.Mmap(int(f.Fd()), 0, int(maxBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
  	if err != nil {
  		f.Close()
  		return nil, err
  	}
  	_ = unix.Madvise(data, unix.MADV_SEQUENTIAL)
  	return &SpanBuffer{
  		dir:      dir,
  		maxBytes: maxBytes,
  		f:        f,
  		data:     data,
  		entries:  make(map[string][]deltaRecord),
  	}, nil
  }

  func (b *SpanBuffer) Close() error {
  	b.mu.Lock()
  	data := b.data
  	if data == nil {
  		b.mu.Unlock()
  		return fmt.Errorf("already closed")
  	}
  	b.data = nil
  	f := b.f
  	b.f = nil
  	b.mu.Unlock()
  	if err := unix.Munmap(data); err != nil {
  		f.Close()
  		return err
  	}
  	return f.Close()
  }

  func (b *SpanBuffer) WriteWithEviction(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
  	b.mu.Lock()
  	defer b.mu.Unlock()

  	m := ptrace.ProtoMarshaler{}
  	delta, err := m.MarshalTraces(spans)
  	if err != nil {
  		return err
  	}

  	recSize := int64(hdrSize + len(delta))
  	if recSize > b.maxBytes {
  		return fmt.Errorf("record size %d exceeds ring capacity %d", recSize, b.maxBytes)
  	}

  	if b.wHead+recSize > b.maxBytes {
  		b.wrapLocked()
  	}

  	for b.used+recSize > b.maxBytes {
  		if err := b.sweepOneLocked(); err != nil {
  			return err
  		}
  	}

  	rec := make([]byte, hdrSize+len(delta))
  	copy(rec[:32], traceID)
  	binary.BigEndian.PutUint64(rec[32:40], uint64(insertedAt.UnixNano()))
  	binary.BigEndian.PutUint32(rec[40:44], uint32(len(delta)))
  	copy(rec[hdrSize:], delta)

  	offset := b.wHead
  	copy(b.data[offset:], rec)

  	b.entries[traceID] = append(b.entries[traceID], deltaRecord{offset, int32(len(delta))})
  	b.wHead += recSize
  	b.used += recSize
  	return nil
  }

  func (b *SpanBuffer) wrapLocked() {
  	remaining := b.maxBytes - b.wHead
  	if remaining >= hdrSize {
  		var hdr [hdrSize]byte
  		binary.BigEndian.PutUint32(hdr[40:44], uint32(remaining-hdrSize))
  		copy(b.data[b.wHead:], hdr[:])
  	}
  	b.used += remaining
  	b.wHead = 0
  }

  func (b *SpanBuffer) sweepOneLocked() error {
  	if b.rHead >= b.maxBytes {
  		b.rHead = 0
  	}
  	if b.rHead+hdrSize > b.maxBytes {
  		remaining := b.maxBytes - b.rHead
  		b.used -= remaining
  		b.rHead = 0
  		b.madviseSwept()
  		return nil
  	}

  	hdr := b.data[b.rHead : b.rHead+hdrSize]
  	dataLen := int64(binary.BigEndian.Uint32(hdr[40:44]))
  	recSize := int64(hdrSize) + dataLen

  	if bytes.Equal(hdr[:32], zeroID[:]) {
  		b.used -= recSize
  		b.rHead = 0
  		b.madviseSwept()
  		return nil
  	}

  	traceID := string(bytes.TrimRight(hdr[:32], "\x00"))
  	if deltas, ok := b.entries[traceID]; ok && len(deltas) > 0 && deltas[0].offset == b.rHead {
  		b.entries[traceID] = deltas[1:]
  		if len(b.entries[traceID]) == 0 {
  			delete(b.entries, traceID)
  		}
  	}

  	b.used -= recSize
  	b.rHead += recSize
  	if b.rHead >= b.maxBytes {
  		b.rHead = 0
  	}
  	b.madviseSwept()
  	return nil
  }

  // madviseSwept releases swept pages from RSS. Called after every rHead
  // advancement; only issues the syscall when rHead crosses a page boundary.
  // On wrap (rHead==0), releases from lastMadvise to end of file.
  func (b *SpanBuffer) madviseSwept() {
  	if b.rHead == 0 {
  		end := b.maxBytes &^ (mmapPageSize - 1)
  		if end > b.lastMadvise {
  			_ = unix.Madvise(b.data[b.lastMadvise:end], unix.MADV_DONTNEED)
  		}
  		b.lastMadvise = 0
  		return
  	}
  	aligned := b.rHead &^ (mmapPageSize - 1)
  	if aligned > b.lastMadvise {
  		_ = unix.Madvise(b.data[b.lastMadvise:aligned], unix.MADV_DONTNEED)
  		b.lastMadvise = aligned
  	}
  }

  func (b *SpanBuffer) Read(traceID string) (ptrace.Traces, bool, error) {
  	b.mu.Lock()
  	defer b.mu.Unlock()

  	deltas := b.entries[traceID]
  	if len(deltas) == 0 {
  		return ptrace.Traces{}, false, nil
  	}

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

  func (b *SpanBuffer) Delete(traceID string) error {
  	b.mu.Lock()
  	defer b.mu.Unlock()
  	delete(b.entries, traceID)
  	return nil
  }
  ```

- [ ] **Step 2: Promote golang.org/x/sys to direct dependency**

  ```bash
  cd processor/retroactivesampling && go mod tidy
  ```

  Expected change in `go.mod`: `golang.org/x/sys v0.43.0` moves from the `// indirect` block to the direct `require` block.

- [ ] **Step 3: Run tests — confirm all 10 pass**

  ```bash
  cd processor/retroactivesampling && go test ./internal/buffer/... -v
  ```

  Expected: same 10 tests as after Task 1, all PASS.

- [ ] **Step 4: Commit**

  ```bash
  git add processor/retroactivesampling/internal/buffer/buffer.go \
          processor/retroactivesampling/go.mod \
          processor/retroactivesampling/go.sum
  git commit -m "feat(buffer): replace file IO with mmap, drop checkpoint"
  ```
