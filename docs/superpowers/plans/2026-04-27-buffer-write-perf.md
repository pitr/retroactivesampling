# Buffer write-coalescing staging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Drop `SpanBuffer.Write` from ~5119 ns/op (6 allocs) to ≤500 ns/op (0 allocs) on the steady-state-full benchmark, by amortising the `Pwrite` syscall via a small in-memory staging buffer and moving eviction inline so the writer↔sweeper context switch is removed from the hot path.

**Architecture:** Add a fixed-size staging buffer (default 4 KiB) on `SpanBuffer`. Writes append to the stage under a brief mutex; when the stage is full or about to wrap on disk, the writer flushes it with a single `unix.Pwrite` and, if the disk ring is full, evicts records inline with a batched `ReadAt`. Sweeper goroutine is reduced to delivery-only (interest-driven). Wrap padding is performed by appending a skip-record to the stage before flush.

**Tech Stack:** Go, `golang.org/x/sys/unix`, `sync.Mutex`/`sync.Cond`, OTel collector pdata.

**Spec:** `docs/superpowers/specs/2026-04-27-buffer-write-perf-design.md`

---

## File Map

- **Modify:** `processor/retroactivesampling/internal/buffer/buffer.go` — primary changes (struct fields, New signature, Write path, flushLocked, evictBatchedLocked, sweeper, Close).
- **Modify:** `processor/retroactivesampling/internal/buffer/buffer_test.go` — update helper for new New signature; new tests for staging behavior.
- **Modify:** `processor/retroactivesampling/internal/buffer/buffer_bench_test.go` — update for new New signature; new benchmarks.
- **Modify:** `processor/retroactivesampling/processor.go` — pass new `stageCap` argument (use `buffer.DefaultStageCap`).

No new files. The change set is tightly coupled and lives in one package.

## Constants & Defaults

Add to `buffer.go`:

```go
const DefaultStageCap = 4096       // 4 KiB staging buffer
const evictScanCap   = 4096        // batch-read window for header scans
```

`stageCap` is exposed as a parameter to `New` so tests can use small values.

## Working Directory

Run all `go` commands from `/Users/p.vernigorov/code/retroactivesampling/processor/retroactivesampling`.

---

## Task 1: Add `stageCap` parameter and constants (no behavior change)

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go` — add constants, add parameter to `New`, validate.
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go:86-96` — `newBuf` helper passes `DefaultStageCap`.
- Modify: `processor/retroactivesampling/internal/buffer/buffer_bench_test.go` — three call sites pass `DefaultStageCap`.
- Modify: `processor/retroactivesampling/processor.go:46-54` — pass `buffer.DefaultStageCap`.

- [ ] **Step 1: Write the failing test**

In `buffer_test.go` add:

```go
func TestNewRejectsTinyStageCap(t *testing.T) {
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, time.Second, buffer.DefaultStageCap, nil, nil)
	require.NoError(t, err)
	_, err = buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, time.Second, 8, nil, nil)
	assert.Error(t, err)
}

func TestNewRejectsStageCapTooLarge(t *testing.T) {
	// stageCap > maxBytes/2 is rejected
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1024, time.Second, 800, nil, nil)
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails to compile (signature mismatch)**

Run: `go test ./internal/buffer/ -run TestNewRejectsTinyStageCap -count=1`
Expected: build failure — `New` does not accept that many arguments.

- [ ] **Step 3: Add constants and update `New` signature**

Replace the existing constants block at the top of `buffer.go`:

```go
const (
	hdrSize        = 28      // traceID(16) + insertedAt(8) + dataLen(4)
	maxPoolPayload = 1 << 16 // 64 KiB; larger slices are not returned to the pool

	DefaultStageCap = 4096   // default in-memory staging buffer size
	evictScanCap    = 4096   // batched header read window in evictBatchedLocked
)
```

Replace the `New` function signature and validation block:

```go
func New(
	file string,
	maxBytes int64,
	decisionWait time.Duration,
	stageCap int,
	onMatch func(pcommon.TraceID, []byte),
	evictObs func(time.Duration),
) (*SpanBuffer, error) {
	if onMatch == nil {
		onMatch = func(pcommon.TraceID, []byte) {}
	}
	if evictObs == nil {
		evictObs = func(time.Duration) {}
	}
	if decisionWait <= 0 {
		return nil, fmt.Errorf("decisionWait must be positive, got %v", decisionWait)
	}
	if maxBytes < hdrSize*2 {
		return nil, fmt.Errorf("maxBytes must be at least %d, got %d", hdrSize*2, maxBytes)
	}
	if stageCap < hdrSize*2 {
		return nil, fmt.Errorf("stageCap must be at least %d, got %d", hdrSize*2, stageCap)
	}
	if int64(stageCap)*2 > maxBytes {
		return nil, fmt.Errorf("stageCap (%d) must be at most maxBytes/2 (%d)", stageCap, maxBytes/2)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	// (rest of New unchanged for now — fields added in Task 2)
```

- [ ] **Step 4: Update existing callers**

In `buffer_test.go`, change `newBuf`:

```go
func newBuf(t *testing.T, maxBytes int64, decisionWait time.Duration, col *collector, evictObs func(time.Duration)) *buffer.SpanBuffer {
	t.Helper()
	var onMatch func(pcommon.TraceID, []byte)
	if col != nil {
		onMatch = col.onMatch
	}
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), maxBytes, decisionWait, buffer.DefaultStageCap, onMatch, evictObs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}
```

In `buffer_test.go` update `TestNewCreatesFileAtPath`, `TestNewRejectsZeroMaxBytes`, `TestNewRejectsZeroDecisionWait`, the `TestEvictionUnderPressure`, and any other direct `buffer.New(` call to include `buffer.DefaultStageCap` between `decisionWait` and `nil`:

```go
buf, err := buffer.New(path, 1<<20, time.Second, buffer.DefaultStageCap, nil, nil)
```

In `buffer_bench_test.go`, update `newBufB` and the inline `buffer.New` call in `BenchmarkRead`:

```go
buf, err := buffer.New(filepath.Join(b.TempDir(), "buf.ring"), maxBytes, time.Hour, buffer.DefaultStageCap, nil, nil)
// ...
buf, err := buffer.New(
	filepath.Join(b.TempDir(), "buf.ring"),
	int64(b.N)*rs+rs,
	time.Hour,
	buffer.DefaultStageCap,
	onMatch,
	nil,
)
```

In `processor.go:46-54`:

```go
buf, err := buffer.New(
	cfg.BufferFile,
	cfg.MaxBufferBytes,
	cfg.DecisionWaitTime,
	buffer.DefaultStageCap,
	p.onMatch,
	func(d time.Duration) {
		tb.RetroactiveSamplingBufferSpanAgeOnEviction.Record(context.Background(), int64(d.Seconds()))
	},
)
```

- [ ] **Step 5: Run all package tests**

Run: `go test ./internal/buffer/ -count=1 -race`
Expected: PASS (including the two new tests).

Run: `go build ./...`
Expected: clean build.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go \
        processor/retroactivesampling/internal/buffer/buffer_test.go \
        processor/retroactivesampling/internal/buffer/buffer_bench_test.go \
        processor/retroactivesampling/processor.go
git commit -m "refactor(buffer): add stageCap parameter to New (no behavior change)"
```

---

## Task 2: Add staging fields to `SpanBuffer` (allocated, unused)

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go` — struct fields, allocate in New.

- [ ] **Step 1: Update struct definition**

Replace the `SpanBuffer` struct in `buffer.go`:

```go
type SpanBuffer struct {
	maxBytes        int64
	f               *os.File
	wHead           int64
	rHead           int64
	used            int64 // disk-live bytes + len(stage)
	decisionWait    time.Duration
	interestEntries map[pcommon.TraceID]*list.Element
	interestList    list.List
	onMatch         func(pcommon.TraceID, []byte)
	evictObs        func(time.Duration)
	fd              int

	// Staging
	stage    []byte
	stageCap int
	scratch  []byte // batched header reads in evictBatchedLocked

	// Concurrency
	mu          sync.Mutex
	flushDone   *sync.Cond    // signals flush slot release / rHead advance
	flushing    bool          // a writer owns the flush slot
	wakeC       chan struct{} // writer/AddInterest → sweeper kick (buffered 1)
	closeC      chan struct{}
	sweeperDone chan struct{}
	closed      bool
}
```

The previous `cond` field is replaced by `flushDone`. We will rename all existing references in subsequent steps within this task.

- [ ] **Step 2: Update `New` to allocate staging fields**

Replace the trailing struct literal in `New`:

```go
	b := &SpanBuffer{
		maxBytes:        maxBytes,
		f:               f,
		fd:              int(f.Fd()),
		decisionWait:    decisionWait,
		interestEntries: make(map[pcommon.TraceID]*list.Element),
		onMatch:         onMatch,
		evictObs:        evictObs,
		stage:           make([]byte, 0, stageCap),
		stageCap:        stageCap,
		scratch:         make([]byte, evictScanCap),
		wakeC:           make(chan struct{}, 1),
		closeC:          make(chan struct{}),
		sweeperDone:     make(chan struct{}),
	}
	b.flushDone = sync.NewCond(&b.mu)
	go b.runSweeper()
	return b, nil
```

- [ ] **Step 3: Replace all `b.cond` with `b.flushDone`**

In `buffer.go`, the `cond.Broadcast()` and `cond.Wait()` calls in `Write`, `Close`, and `runSweeper` need updating. For now, mechanical rename — semantics preserved:

- `b.cond.Broadcast()` → `b.flushDone.Broadcast()`
- `b.cond.Wait()` → `b.flushDone.Wait()`
- `b.cond = sync.NewCond(&b.mu)` removed (already replaced in Step 2).

There are 4 call sites: `Write` (Wait + 2 Broadcasts), `Close` (Broadcast), `runSweeper` (3 Broadcasts).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/buffer/ -count=1 -race`
Expected: PASS — behavior unchanged.

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "refactor(buffer): add staging fields, rename cond to flushDone"
```

---

## Task 3: Reject records that exceed `stageCap`

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go` — add stageCap check at top of `Write`.
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go` — new test.

- [ ] **Step 1: Write the failing test**

In `buffer_test.go` add:

```go
func TestWriteRejectsRecordLargerThanStageCap(t *testing.T) {
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, time.Second, 256, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	// Payload + 28-byte header > stageCap=256.
	big := make([]byte, 300)
	err = buf.Write(traceA, big, time.Now())
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/buffer/ -run TestWriteRejectsRecordLargerThanStageCap -count=1`
Expected: FAIL — Write currently accepts records up to maxBytes.

- [ ] **Step 3: Implement the check**

In `Write`, after the existing `recSize > b.maxBytes` check, add:

```go
	if recSize > int64(b.stageCap) {
		return fmt.Errorf("record size %d exceeds stage capacity %d", recSize, b.stageCap)
	}
```

- [ ] **Step 4: Run the failing test and full suite**

Run: `go test ./internal/buffer/ -count=1 -race`
Expected: PASS (including the new test).

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "feat(buffer): reject records larger than stageCap"
```

---

## Task 4: Implement `flushLocked` and switch `Write` hot path to staging

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go` — replace `Write` body, add `flushLocked`, add `evictBatchedLocked`, simplify sweeper.

This is the load-bearing task. The change is atomic: writer goes from disk-per-call to stage-per-call; eviction moves from sweeper into writer; sweeper becomes delivery-only. Existing tests must keep passing.

- [ ] **Step 1: Replace `Write` body**

Replace `Write` in `buffer.go`:

```go
// Write appends a record to the staging buffer. If the stage is full the
// record is flushed to disk first; if the disk ring is full the writer
// evicts records inline.
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	recSize := hdrSize + int64(len(data))
	if recSize > b.maxBytes {
		return fmt.Errorf("record size %d exceeds ring capacity %d", recSize, b.maxBytes)
	}
	if recSize > int64(b.stageCap) {
		return fmt.Errorf("record size %d exceeds stage capacity %d", recSize, b.stageCap)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("buffer closed")
	}

	// Flush if appending would cross the disk wrap boundary.
	if b.wHead+int64(len(b.stage))+recSize > b.maxBytes {
		if err := b.flushLocked(true); err != nil {
			return err
		}
	}

	// Flush if stage is full.
	if int64(len(b.stage))+recSize > int64(b.stageCap) {
		if err := b.flushLocked(false); err != nil {
			return err
		}
	}

	if b.closed {
		return fmt.Errorf("buffer closed")
	}

	// Append record to stage.
	stageOff := len(b.stage)
	b.stage = b.stage[:stageOff+int(recSize)]
	hdr := b.stage[stageOff : stageOff+hdrSize]
	copy(hdr[:16], traceID[:])
	binary.BigEndian.PutUint64(hdr[16:24], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(hdr[24:28], uint32(len(data)))
	copy(b.stage[stageOff+hdrSize:], data)
	b.used += recSize

	return nil
}
```

- [ ] **Step 2: Add `flushLocked`**

Insert immediately after `Write`:

```go
// flushLocked drains the staging buffer to disk in a single Pwrite, evicting
// records inline if the ring lacks space. If wrap=true the stage is padded
// with a skip-record so that wHead lands exactly at maxBytes (then wraps to
// 0). Caller holds b.mu.
func (b *SpanBuffer) flushLocked(wrap bool) error {
	for b.flushing && !b.closed {
		b.flushDone.Wait()
	}
	if b.closed {
		return fmt.Errorf("buffer closed")
	}
	if len(b.stage) == 0 {
		return nil
	}
	b.flushing = true
	defer func() {
		b.flushing = false
		b.flushDone.Broadcast()
	}()

	// Wrap padding: append a skip-record header so the on-disk write tiles
	// the remaining tail space exactly.
	if wrap {
		gap := b.maxBytes - b.wHead - int64(len(b.stage))
		if gap >= hdrSize {
			off := len(b.stage)
			padTotal := int(gap)
			b.stage = b.stage[:off+padTotal]
			pad := b.stage[off : off+hdrSize]
			for i := range pad[:16] {
				pad[i] = 0
			}
			binary.BigEndian.PutUint64(pad[16:24], 0)
			binary.BigEndian.PutUint32(pad[24:28], uint32(padTotal-hdrSize))
			// pad bytes after the header are not zeroed; sweeper skips them via dataLen.
			b.used += int64(padTotal)
		} else if gap > 0 {
			// Tail gap < hdrSize: leave it; sweeper's tail-gap rule advances rHead past it.
			b.used += gap
		}
	}

	flushLen := int64(len(b.stage))

	// Ensure disk has flushLen bytes free at wHead.
	if err := b.evictBatchedLocked(flushLen); err != nil {
		return err
	}

	// Pwrite with the lock released.
	off := b.wHead
	buf := b.stage // safe: only this writer owns stage during flush
	b.mu.Unlock()
	n, err := unix.Pwrite(b.fd, buf, off)
	b.mu.Lock()

	if err != nil {
		return err
	}
	if int64(n) != flushLen {
		return fmt.Errorf("short pwrite: got %d, want %d", n, flushLen)
	}

	b.wHead += flushLen
	if wrap {
		// After wrap-flush wHead == maxBytes; reset to 0.
		b.wHead = 0
	} else if b.wHead == b.maxBytes {
		b.wHead = 0
	}
	b.stage = b.stage[:0]
	return nil
}
```

- [ ] **Step 3: Add `evictBatchedLocked`**

Insert after `flushLocked`:

```go
// evictBatchedLocked frees `need` bytes at the disk wHead by advancing rHead
// through uninteresting records. Interesting records are handed off to the
// sweeper for delivery; the writer waits on flushDone until the sweeper
// advances past them. Caller holds b.mu and owns the flush slot.
func (b *SpanBuffer) evictBatchedLocked(need int64) error {
	for {
		// Free disk space = maxBytes - (used - len(stage)).
		freeDisk := b.maxBytes - (b.used - int64(len(b.stage)))
		if freeDisk >= need {
			return nil
		}

		// Tail gap: fewer than hdrSize bytes before wrap. Skip to 0.
		if b.rHead+hdrSize > b.maxBytes {
			b.used -= b.maxBytes - b.rHead
			b.rHead = 0
			continue
		}

		// Batched read of headers starting at rHead.
		readLen := evictScanCap
		if remain := b.maxBytes - b.rHead; int64(readLen) > remain {
			readLen = int(remain)
		}
		buf := b.scratch[:readLen]
		off := b.rHead
		b.mu.Unlock()
		n, err := b.f.ReadAt(buf, off)
		b.mu.Lock()
		if err != nil && n == 0 {
			return err
		}
		buf = buf[:n]

		// Walk records inside the buffer.
		for pos := 0; pos+hdrSize <= len(buf); {
			hdr := buf[pos : pos+hdrSize]
			var tid pcommon.TraceID
			copy(tid[:], hdr[:16])
			dataLen := int64(binary.BigEndian.Uint32(hdr[24:28]))
			recSize := hdrSize + dataLen

			// Skip-record (zero traceID): drop it.
			if tid.IsEmpty() {
				b.used -= recSize
				b.rHead += recSize
				if b.rHead >= b.maxBytes {
					b.rHead = 0
				}
				pos += int(recSize)
				freeDisk = b.maxBytes - (b.used - int64(len(b.stage)))
				if freeDisk >= need {
					return nil
				}
				continue
			}

			// Stop if record extends beyond the read window — re-read on next iteration.
			if pos+int(recSize) > len(buf) {
				break
			}

			if b.hasInterestLocked(tid) {
				// Hand off to sweeper, wait for it to advance rHead past this record.
				targetRHead := b.rHead + recSize
				select {
				case b.wakeC <- struct{}{}:
				default:
				}
				for b.rHead < targetRHead && !b.closed {
					b.flushDone.Wait()
				}
				if b.closed {
					return fmt.Errorf("buffer closed")
				}
				// rHead advanced; re-evaluate from top of outer loop (geometry changed).
				break
			}

			insertedAt := time.Unix(0, int64(binary.BigEndian.Uint64(hdr[16:24])))
			b.evictObs(time.Since(insertedAt))
			b.used -= recSize
			b.rHead += recSize
			if b.rHead >= b.maxBytes {
				b.rHead = 0
				break // re-read at offset 0 next iteration
			}
			pos += int(recSize)

			freeDisk = b.maxBytes - (b.used - int64(len(b.stage)))
			if freeDisk >= need {
				return nil
			}
		}
	}
}
```

- [ ] **Step 4: Replace the sweeper with delivery-only logic**

Replace `runSweeper` in `buffer.go`:

```go
func (b *SpanBuffer) runSweeper() {
	defer close(b.sweeperDone)
	for {
		// Wait for a kick.
		select {
		case <-b.wakeC:
		case <-b.closeC:
			return
		}

		// Drain: scan disk records [rHead, wHead) for deliverable interesting records.
		for {
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				return
			}
			// Bytes on disk = used - len(stage).
			diskUsed := b.used - int64(len(b.stage))
			if diskUsed <= 0 {
				b.mu.Unlock()
				break
			}
			rHead := b.rHead

			// Tail gap: skip to 0.
			if rHead+hdrSize > b.maxBytes {
				b.used -= b.maxBytes - rHead
				b.rHead = 0
				b.flushDone.Broadcast()
				b.mu.Unlock()
				continue
			}
			b.mu.Unlock()

			var hdr [hdrSize]byte
			if _, err := b.f.ReadAt(hdr[:], rHead); err != nil {
				return
			}

			var tid pcommon.TraceID
			copy(tid[:], hdr[:16])
			dataLen := int64(binary.BigEndian.Uint32(hdr[24:28]))
			recSize := hdrSize + dataLen

			if tid.IsEmpty() {
				b.mu.Lock()
				b.used -= recSize
				b.rHead += recSize
				if b.rHead >= b.maxBytes {
					b.rHead = 0
				}
				b.flushDone.Broadcast()
				b.mu.Unlock()
				continue
			}

			b.mu.Lock()
			interesting := b.hasInterestLocked(tid)
			b.mu.Unlock()

			if !interesting {
				// Sweeper does NOT advance rHead past uninteresting records — that's
				// the writer's eviction job. We're done with this drain pass; wait
				// for the next kick.
				break
			}

			// Deliver.
			pb := payloadPool.Get().(*[]byte)
			if int64(cap(*pb)) < dataLen {
				*pb = make([]byte, dataLen)
			} else {
				*pb = (*pb)[:dataLen]
			}
			if _, err := b.f.ReadAt(*pb, rHead+hdrSize); err == nil {
				b.onMatch(tid, *pb)
			}
			if cap(*pb) <= maxPoolPayload {
				payloadPool.Put(pb)
			}

			insertedAt := time.Unix(0, int64(binary.BigEndian.Uint64(hdr[16:24])))
			b.mu.Lock()
			b.used -= recSize
			b.rHead += recSize
			if b.rHead >= b.maxBytes {
				b.rHead = 0
			}
			b.evictObs(time.Since(insertedAt))
			b.flushDone.Broadcast()
			b.mu.Unlock()
		}
	}
}
```

Note: the sweeper now stops at the first uninteresting record — the writer is responsible for eviction. The existing `TestEvictionObserver` and `TestDecisionWaitHonoured` tests rely on the sweeper advancing past uninteresting records to call `evictObs`. Those tests will break and are fixed in Task 5.

- [ ] **Step 5: Update `Close` to flush stage**

Replace `Close`:

```go
func (b *SpanBuffer) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("already closed")
	}
	// Flush any pending stage so sweeper can deliver outstanding interesting records.
	if len(b.stage) > 0 {
		// Flush without wrap consideration; if wrap is needed it'll happen here
		// just like in Write. Compute the wrap flag once.
		wrap := b.wHead+int64(len(b.stage)) > b.maxBytes
		_ = b.flushLocked(wrap) // best-effort; ignore error to ensure Close progresses
	}
	b.closed = true
	b.flushDone.Broadcast()
	b.mu.Unlock()
	close(b.closeC)
	<-b.sweeperDone
	return b.f.Close()
}
```

- [ ] **Step 6: Build**

Run: `go build ./...`
Expected: clean build. If type errors appear (e.g., from removed code), fix mechanically — the changes above replace `Write`, `Close`, and `runSweeper` wholesale.

- [ ] **Step 7: Run buffer tests**

Run: `go test ./internal/buffer/ -count=1 -race -run '^TestNew|TestInterestDelivery|TestMultipleDeltas|TestHasInterest|TestWrap|TestWriteRejects'`
Expected: PASS for these tests (they don't depend on sweeper-driven eviction).

Run: `go test ./internal/buffer/ -count=1 -race -run TestEviction`
Expected: `TestEvictionObserver` may FAIL because the sweeper no longer advances past uninteresting records under no write pressure. This is expected and fixed in Task 5.

Run: `go test ./internal/buffer/ -count=1 -race -run TestEvictionUnderPressure`
Expected: PASS — under write pressure the writer evicts inline.

- [ ] **Step 8: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "feat(buffer): coalesce writes via staging buffer; writer evicts inline

Adds in-memory stage (default 4 KiB) on the write path. Hot path is now
memcpy + arithmetic under the mutex; flush runs once per stage-full and
issues a single Pwrite. Writer evicts uninteresting records inline via
batched ReadAt; sweeper handles delivery only.

Two existing tests (TestEvictionObserver, TestDecisionWaitHonoured) rely
on sweeper-driven eviction without write pressure; they will be reframed
to drive eviction via writes in the next commit."
```

---

## Task 5: Reframe age-based tests to drive eviction via writes

The two age-based tests assumed the sweeper independently drops uninteresting records after `decisionWait`. With writer-driven eviction, drops happen on write pressure or on Close. Reframe the tests so a write triggers the eviction.

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go:142-181`.

- [ ] **Step 1: Replace `TestEvictionObserver`**

In `buffer_test.go`, replace the existing `TestEvictionObserver` function:

```go
// TestEvictionObserver: writer-driven eviction calls evictObs with the record's age.
func TestEvictionObserver(t *testing.T) {
	observed := make(chan time.Duration, 4)
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	rs := recSize(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))

	// Two-record ring with default stageCap. First write fills the ring; second
	// triggers writer-driven eviction.
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 2*rs, 10*time.Millisecond, buffer.DefaultStageCap, nil, func(d time.Duration) {
		select {
		case observed <- d:
		default:
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	require.NoError(t, buf.Write(traceA, data, time.Now().Add(-20*time.Millisecond)))
	require.NoError(t, buf.Write(traceB, data, time.Now().Add(-20*time.Millisecond)))
	// Third write requires eviction. Stage flushes after second write may already
	// have triggered eviction depending on stage size; force one more write.
	require.NoError(t, buf.Write(traceC, data, time.Now()))
	// Close flushes outstanding records — guarantees evictObs has fired.
	require.NoError(t, buf.Close())

	select {
	case d := <-observed:
		assert.Greater(t, d, time.Duration(0))
	case <-time.After(2 * time.Second):
		t.Fatal("evictObserver not called")
	}
}
```

Note: this test calls `buf.Close()` directly, so remove the deferred `t.Cleanup` close call — actually, leave it, because Close is idempotent-safe-ish (we return error on second Close). Adjust: change `Cleanup` to use `_ = buf.Close()` (which it already does — already swallows error). Keep as-is.

Actually the current cleanup has `_ = buf.Close()` so the second call returns "already closed" but is silently dropped. Fine.

- [ ] **Step 2: Replace `TestDecisionWaitHonoured`**

Replace it with a test of the new contract: under no write pressure, uninteresting records remain on disk (not delivered, not dropped by sweeper). They are eventually evicted by writer pressure or Close.

```go
// TestUninterestingRecordRemainsUntilPressure: with no write pressure and no
// interest, the record sits in the buffer; only writer-driven eviction (or
// Close) discards it.
func TestUninterestingRecordRemainsUntilPressure(t *testing.T) {
	observed := make(chan time.Duration, 1)
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	rs := recSize(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 2*rs, 10*time.Millisecond, buffer.DefaultStageCap, nil, func(d time.Duration) {
		select {
		case observed <- d:
		default:
		}
	})
	require.NoError(t, err)

	require.NoError(t, buf.Write(traceA, data, time.Now()))
	// No further writes; sweeper must not drop the record on its own.
	select {
	case <-observed:
		t.Fatal("uninteresting record was dropped without write pressure")
	case <-time.After(50 * time.Millisecond):
	}

	// Close flushes and forces eviction during shutdown.
	require.NoError(t, buf.Close())
}
```

Wait — the new sweeper does not evict on Close either. Records on disk at Close are simply discarded with the file. evictObs won't fire on Close. Adjust: this test asserts only that `observed` does NOT fire under no pressure; it does not require Close to fire it. The negative assertion is the only behavior check.

Also note that with `decisionWait` no longer driving sweeper drops, the parameter remains in the interest-set TTL only. That's a semantic change — call it out in commit.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/buffer/ -count=1 -race`
Expected: PASS for all tests.

- [ ] **Step 4: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "test(buffer): reframe age-based tests for writer-driven eviction

decisionWait no longer triggers sweeper-driven drops; it remains the
interest-set TTL only. Eviction is now writer-driven and happens under
ring pressure or never."
```

---

## Task 6: New test — staging amortizes Pwrite

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go` — add `flushForTest` (build-tag-free package-internal helper) and `pendingFlushBytes()` getter for tests.
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go` — new test.

To keep the `buffer` package's API clean, expose the test helpers via an internal accessor file. Simplest: add an unexported method documented as test-only.

- [ ] **Step 1: Add test helpers to `buffer.go`**

Add at the bottom of `buffer.go`:

```go
// stageLenForTest returns the number of bytes currently buffered in the stage.
// Test-only: not part of the public contract.
func (b *SpanBuffer) stageLenForTest() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.stage)
}
```

To make this visible from `buffer_test.go` (which is in `package buffer_test`), expose it through an internal export file that lives in `package buffer`. Simpler: rename to `StageLenForTest` and document as test-only. Alternative: put the new test in `package buffer` (white-box) by creating `internal_test.go`.

Pick: use a white-box test file `internal_test.go` (in `package buffer`) for tests that need internal access. This keeps the public API clean.

Create `processor/retroactivesampling/internal/buffer/internal_test.go`:

```go
package buffer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

func TestStagingAmortizesFlush(t *testing.T) {
	traceA := pcommon.TraceID([16]byte{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa})

	buf, err := New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, time.Hour, 4096, nil, nil)
	require.NoError(t, err)
	defer func() { _ = buf.Close() }()

	payload := make([]byte, 100)
	recSize := hdrSize + len(payload) // 28 + 100 = 128
	maxRecordsInStage := 4096 / recSize

	for i := 0; i < maxRecordsInStage; i++ {
		require.NoError(t, buf.Write(traceA, payload, time.Now()))
	}

	buf.mu.Lock()
	stagedBytes := len(buf.stage)
	wHead := buf.wHead
	buf.mu.Unlock()

	require.Greater(t, stagedBytes, 0, "stage should hold records before overflow")
	require.Equal(t, int64(0), wHead, "no flush should have happened yet")

	// One more write triggers flush.
	require.NoError(t, buf.Write(traceA, payload, time.Now()))

	buf.mu.Lock()
	wHead = buf.wHead
	stagedBytes = len(buf.stage)
	buf.mu.Unlock()

	require.Greater(t, wHead, int64(0), "flush should have advanced wHead")
	require.Equal(t, recSize, stagedBytes, "only the most recent record is in stage")
}
```

(Remove the helper method addition; the white-box test reaches in directly.)

- [ ] **Step 2: Run the test**

Run: `go test ./internal/buffer/ -count=1 -race -run TestStagingAmortizesFlush`
Expected: PASS.

- [ ] **Step 3: Run full suite**

Run: `go test ./internal/buffer/ -count=1 -race`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/internal_test.go
git commit -m "test(buffer): verify staging amortizes Pwrite syscalls"
```

---

## Task 7: New test — AddInterest after Write delivers post-flush

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go` — new test.

- [ ] **Step 1: Write the test**

Add to `buffer_test.go`:

```go
// TestAddInterestOnStagedRecord: AddInterest fired while record is in stage;
// closing the buffer flushes stage and delivers the record.
func TestAddInterestOnStagedRecord(t *testing.T) {
	col := newCollector()
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, 5*time.Second, buffer.DefaultStageCap, col.onMatch, nil)
	require.NoError(t, err)

	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(t, buf.Write(traceA, data, time.Now()))
	// Record is in stage; no flush yet.

	buf.AddInterest(traceA)

	// Force a flush by closing.
	require.NoError(t, buf.Close())

	chunks := col.get(traceA)
	assert.Len(t, chunks, 1)
	assert.Equal(t, data, chunks[0])
}
```

Note: closing waits for sweeper to exit, but the sweeper should have processed the kick from AddInterest and the post-Close flush. Confirm that Close's sequence (flush stage → set closed → broadcast → close(closeC) → wait sweeperDone) gives the sweeper one more chance to drain. The current Close in Task 4 sets `closed=true` after flush but BEFORE close(closeC). Sweeper checks `closed` inside the drain loop and exits. We need the sweeper to drain the just-flushed records before exiting on close.

Adjust Close (in Task 4) so it kicks the sweeper after flush and waits for drain before signaling close. Actually the cleanest path: AddInterest already kicks `wakeC`. After flush, the stage's records are on disk. The sweeper needs another wakeC kick. Have Close kick wakeC after flush, then wait briefly, then close. Simpler: have the sweeper drain on closeC before exiting.

Re-read Task 4 Step 4 sweeper: on closeC it returns immediately. Need to do one final drain pass on close.

Fix Close in Task 4 (apply before this test runs). Add: after flush, kick wakeC and call b.flushDone.Wait() with a check that lets sweeper finish? Cleaner: have the sweeper drain pending interesting records before exit on closeC.

Adjustment: replace the sweeper's outer select with:

```go
select {
case <-b.wakeC:
case <-b.closeC:
	// drain one final time before exiting
}
```

Then continue into the drain loop and exit when no more interesting records or `closed` is true AND nothing left. Use a flag `closing := false` that, once set by closeC, causes the loop to exit after one drain pass.

Concretely, in `runSweeper` (replacing Step 4 of Task 4):

```go
func (b *SpanBuffer) runSweeper() {
	defer close(b.sweeperDone)
	closing := false
	for {
		if !closing {
			select {
			case <-b.wakeC:
			case <-b.closeC:
				closing = true
			}
		}
		// Drain pass.
		for {
			b.mu.Lock()
			diskUsed := b.used - int64(len(b.stage))
			if diskUsed <= 0 {
				b.mu.Unlock()
				break
			}
			rHead := b.rHead
			if rHead+hdrSize > b.maxBytes {
				b.used -= b.maxBytes - rHead
				b.rHead = 0
				b.flushDone.Broadcast()
				b.mu.Unlock()
				continue
			}
			b.mu.Unlock()

			var hdr [hdrSize]byte
			if _, err := b.f.ReadAt(hdr[:], rHead); err != nil {
				return
			}
			var tid pcommon.TraceID
			copy(tid[:], hdr[:16])
			dataLen := int64(binary.BigEndian.Uint32(hdr[24:28]))
			recSize := hdrSize + dataLen

			if tid.IsEmpty() {
				b.mu.Lock()
				b.used -= recSize
				b.rHead += recSize
				if b.rHead >= b.maxBytes {
					b.rHead = 0
				}
				b.flushDone.Broadcast()
				b.mu.Unlock()
				continue
			}

			b.mu.Lock()
			interesting := b.hasInterestLocked(tid)
			b.mu.Unlock()
			if !interesting {
				break
			}

			pb := payloadPool.Get().(*[]byte)
			if int64(cap(*pb)) < dataLen {
				*pb = make([]byte, dataLen)
			} else {
				*pb = (*pb)[:dataLen]
			}
			if _, err := b.f.ReadAt(*pb, rHead+hdrSize); err == nil {
				b.onMatch(tid, *pb)
			}
			if cap(*pb) <= maxPoolPayload {
				payloadPool.Put(pb)
			}

			insertedAt := time.Unix(0, int64(binary.BigEndian.Uint64(hdr[16:24])))
			b.mu.Lock()
			b.used -= recSize
			b.rHead += recSize
			if b.rHead >= b.maxBytes {
				b.rHead = 0
			}
			b.evictObs(time.Since(insertedAt))
			b.flushDone.Broadcast()
			b.mu.Unlock()
		}

		if closing {
			return
		}
	}
}
```

Apply this fix as part of this task before adding the new test (it's a continuation of Task 4's design, surfaced by a real test scenario).

- [ ] **Step 2: Apply the sweeper drain-on-close fix**

Replace `runSweeper` in `buffer.go` with the version above.

- [ ] **Step 3: Run the new test**

Run: `go test ./internal/buffer/ -count=1 -race -run TestAddInterestOnStagedRecord`
Expected: PASS.

- [ ] **Step 4: Run full suite**

Run: `go test ./internal/buffer/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "feat(buffer): sweeper drains once on close so post-flush deliveries land

Adds test for AddInterest fired while record is in stage."
```

---

## Task 8: New test — concurrent writers

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go` — new test.

- [ ] **Step 1: Write the test**

Add to `buffer_test.go`:

```go
// TestConcurrentWritersAllDelivered: 8 writers, all interest-marked, all records delivered.
func TestConcurrentWritersAllDelivered(t *testing.T) {
	const writers = 8
	const perWriter = 100

	col := newCollector()
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<22, 5*time.Second, buffer.DefaultStageCap, col.onMatch, nil)
	require.NoError(t, err)

	tids := make([]pcommon.TraceID, writers)
	for i := range tids {
		tids[i][15] = byte(i + 1) // distinct, non-zero
		buf.AddInterest(tids[i])
	}

	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(tid pcommon.TraceID) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				require.NoError(t, buf.Write(tid, data, time.Now()))
			}
		}(tids[w])
	}
	wg.Wait()

	require.NoError(t, buf.Close())

	for _, tid := range tids {
		assert.Len(t, col.get(tid), perWriter, "missing deliveries for trace %v", tid)
	}
}
```

- [ ] **Step 2: Run with race detector**

Run: `go test ./internal/buffer/ -count=1 -race -run TestConcurrentWritersAllDelivered`
Expected: PASS, no data race.

- [ ] **Step 3: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "test(buffer): concurrent writers deliver all interest-marked records"
```

---

## Task 9: New test — eviction handoff for interesting record

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go` — new test.

- [ ] **Step 1: Write the test**

Add to `buffer_test.go`:

```go
// TestEvictionHandsOffInterestingRecord: interesting record at rHead is delivered
// (not dropped) when writer triggers eviction.
func TestEvictionHandsOffInterestingRecord(t *testing.T) {
	col := newCollector()
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	rs := recSize(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))

	// Two-record ring forces eviction on third write.
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 2*rs, 5*time.Second, buffer.DefaultStageCap, col.onMatch, nil)
	require.NoError(t, err)

	require.NoError(t, buf.Write(traceA, data, time.Now()))
	require.NoError(t, buf.Write(traceB, data, time.Now()))
	// Mark traceA interesting BEFORE the eviction-triggering write.
	buf.AddInterest(traceA)

	// Third write needs eviction; traceA is at rHead and is interesting.
	require.NoError(t, buf.Write(traceC, data, time.Now()))
	// Close drains.
	require.NoError(t, buf.Close())

	assert.Len(t, col.get(traceA), 1, "interesting traceA should be delivered, not dropped")
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/buffer/ -count=1 -race -run TestEvictionHandsOffInterestingRecord -timeout 30s`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "test(buffer): writer eviction hands off interesting records to sweeper"
```

---

## Task 10: New benchmarks — concurrent writers, with delivery

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer_bench_test.go` — add two benchmarks.

- [ ] **Step 1: Add `BenchmarkWriteConcurrent`**

Append to `buffer_bench_test.go`:

```go
// BenchmarkWriteConcurrent measures write throughput under contention.
// 8 goroutines write to a ring sized so eviction is required.
func BenchmarkWriteConcurrent(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(b, err)

	bufSize := int64(64 * 1024) // 64 KiB ring
	buf := newBufB(b, bufSize)
	now := time.Now().Add(-time.Second)

	// Warm the ring near capacity.
	rs := int64(28 + len(data))
	for n := bufSize/rs - 4; n > 0; n-- {
		require.NoError(b, buf.Write(traceA, data, now))
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = buf.Write(traceA, data, now)
		}
	})
}
```

- [ ] **Step 2: Add `BenchmarkWriteWithDelivery`**

Append:

```go
// BenchmarkWriteWithDelivery measures write throughput with 5% interest rate.
// Interesting records require sweeper handoff during eviction.
func BenchmarkWriteWithDelivery(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(b, err)
	rs := int64(28 + len(data))

	bufSize := int64(64 * 1024)
	buf, err := buffer.New(
		filepath.Join(b.TempDir(), "buf.ring"),
		bufSize,
		time.Hour,
		buffer.DefaultStageCap,
		func(_ pcommon.TraceID, _ []byte) {},
		nil,
	)
	require.NoError(b, err)
	b.Cleanup(func() { _ = buf.Close() })

	now := time.Now().Add(-time.Second)

	// Pre-fill near capacity.
	for n := bufSize/rs - 4; n > 0; n-- {
		require.NoError(b, buf.Write(traceA, data, now))
	}

	// Mark every 20th trace ID interesting (5%).
	for i := 0; i < 64; i++ {
		var tid pcommon.TraceID
		binary.BigEndian.PutUint64(tid[8:], uint64(i*20+1))
		buf.AddInterest(tid)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		var tid pcommon.TraceID
		binary.BigEndian.PutUint64(tid[8:], uint64(i+1))
		_ = buf.Write(tid, data, now)
	}
}
```

- [ ] **Step 3: Run benchmarks**

Run: `go test ./internal/buffer/ -bench='BenchmarkWriteConcurrent|BenchmarkWriteWithDelivery' -benchmem -run='^$' -benchtime=1s`
Expected: both run cleanly.

- [ ] **Step 4: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer_bench_test.go
git commit -m "bench(buffer): concurrent writers and 5% delivery rate scenarios"
```

---

## Task 11: Verify primary BenchmarkWrite hits target

**Files:** none (verification only).

- [ ] **Step 1: Run primary benchmark for comparison**

Run: `go test ./internal/buffer/ -bench=BenchmarkWrite$ -benchmem -run='^$' -benchtime=2s`
Expected: ≤500 ns/op, 0 allocs/op (target).

If allocs > 0: profile with `go test ./internal/buffer/ -bench=BenchmarkWrite$ -benchmem -run='^$' -benchtime=2s -memprofile mem.out` and inspect via `go tool pprof -top mem.out`. Common culprits: a slice escape in the hot path; a `sync.Cond` Wait under contention (acceptable since slow path).

If ns/op > 500: profile CPU with `-cpuprofile cpu.out`, inspect with `go tool pprof -top cpu.out`. Likely candidates: mutex contention, Pwrite syscall too frequent (stage too small), eviction batch too small.

- [ ] **Step 2: If target missed, tune `evictScanCap` or `DefaultStageCap`**

If eviction is the bottleneck, raise `evictScanCap` to 8192. If Pwrite is the bottleneck, raise `DefaultStageCap` to 8192. Re-run the benchmark.

- [ ] **Step 3: Run full test suite once more**

Run: `go test ./internal/buffer/ -count=1 -race -timeout 60s`
Expected: PASS.

Run: `go test ./... -count=1 -race -timeout 120s`
Expected: PASS across the processor module (catches unintended changes to processor.go behavior).

- [ ] **Step 4: Capture before/after numbers in commit message**

```bash
git commit --allow-empty -m "perf(buffer): write hot path now <X ns/op, Y allocs (was 5119/6)

Baseline:  BenchmarkWrite-10  416064  5119 ns/op  515 B/op  6 allocs/op
After:     BenchmarkWrite-10  XXXXXX   XXX ns/op   XX B/op  X allocs/op
"
```

(Replace placeholders with measured numbers before running the commit. If a non-empty perf tweak commit happened in Step 2, use that commit's message instead of `--allow-empty`.)

---

## Self-review notes

- **Spec coverage:** All sections of the spec are implemented:
  - Staging buffer + stageCap parameter: Tasks 1, 2.
  - ErrRecordTooLarge for stageCap: Task 3.
  - flushLocked + wrap-pad-in-stage + Pwrite-with-lock-released: Task 4.
  - evictBatchedLocked with batched ReadAt and interest handoff: Task 4.
  - Sweeper delivery-only: Tasks 4, 7.
  - Close flushes stage and lets sweeper drain once: Task 7 fixes the race.
  - decisionWait demoted to interest-set TTL only: documented in Task 5 commit.
  - Tests for staging amortization, AddInterest-on-staged, concurrent writers, eviction handoff, Close drain: Tasks 6–9.
  - Benchmarks: Tasks 10, 11.
  - Observability section of the spec mentions LiveBytes/OrphanedBytes — those getters do not exist in the current `buffer.go` (removed), so no work required.

- **Type consistency:** `flushLocked(wrap bool)` signature is consistent across Tasks 4, 5, 7. `evictBatchedLocked(need int64)` consistent. `stageCap int` parameter consistent. `payloadPool`, `hdrSize`, `maxPoolPayload` constants unchanged.

- **Tasks are bite-sized** (2–5 minutes per step except Task 4, which is one big atomic refactor with 8 steps; this is unavoidable because the staging switch, eviction move, and sweeper change are tightly coupled).
