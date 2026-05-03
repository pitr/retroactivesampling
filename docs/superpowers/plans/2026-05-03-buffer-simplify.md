# SpanBuffer Simplification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the configurable `chunkSize` parameter from `SpanBuffer`, replace it with `os.Getpagesize()` internally, drop out-of-order (priority) delivery, and support records of unbounded size via a multi-chunk write/read path.

**Architecture:** Four sequential tasks: (1) delete priority delivery code, (2) replace `chunkSize` with `pageSize` across all files, (3) implement the large-record write path, (4) implement the multi-chunk sweeper path. Each task is independently compilable and all tests pass after each commit.

**Tech Stack:** Go, `os.Getpagesize()`, `container/list`, `sync.Cond`, `os.File` (WriteAt/ReadAt)

**Spec:** `docs/superpowers/specs/2026-05-02-buffer-simplify-design.md`

---

## File Map

| File | Change |
|------|--------|
| `processor/retroactivesampling/internal/buffer/buffer.go` | Main: remove priority delivery, add `pageSize` var, remove `chunkSize` field/param, add large-record write + sweeper paths |
| `processor/retroactivesampling/internal/buffer/buffer_test.go` | Remove `TestSweeper_crossChunkDelivery`, `TestWrite_recordTooLarge`; update `newBuf` signature; update all sizes to `pageSize`; add 3 new tests |
| `processor/retroactivesampling/internal/buffer/buffer_bench_test.go` | Drop `chunkSize` arg from `buffer.New` |
| `processor/retroactivesampling/config.go` | Remove `ChunkSize int` field |
| `processor/retroactivesampling/factory.go` | Remove `ChunkSize: 4096` from default config |
| `processor/retroactivesampling/processor.go` | Drop `cfg.ChunkSize` from `buffer.New` call |
| `processor/retroactivesampling/processor_test.go` | Remove `ChunkSize: 4096` from two `Config` literals |
| `processor/retroactivesampling/integration_test.go` | Remove `ChunkSize: 4096` from `Config` literal |

All commands below run from `processor/retroactivesampling/` unless noted.

---

## Task 1: Drop priority delivery

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 1.1: Delete `TestSweeper_crossChunkDelivery` from `buffer_test.go`**

Remove the entire test function (lines 219–267 in current file). It tests priority delivery which is being removed.

- [ ] **Step 1.2: Run tests to confirm the deleted test is the only failure**

```bash
go test ./internal/buffer/... -v
```
Expected: PASS (the test was testing a feature, not a bug — deleting it leaves everything green).

- [ ] **Step 1.3: Remove `priorityDelivery` field from `SpanBuffer` struct in `buffer.go`**

Delete this line from the struct:
```go
priorityDelivery map[pcommon.TraceID]struct{}
```

- [ ] **Step 1.4: Remove `priorityDelivery` initialization from `New()`**

Delete this line from the struct literal in `New()`:
```go
priorityDelivery: make(map[pcommon.TraceID]struct{}),
```

- [ ] **Step 1.5: Remove priority signal from `AddInterest()`**

Delete these two lines from `AddInterest()`:
```go
b.priorityDelivery[traceID] = struct{}{}
b.wakeupPending = true
```
Leave the `b.cond.Signal()` line in place — it still serves the normal FIFO wakeup.

After deletion, `AddInterest` ends with:
```go
	b.priorityDelivery[traceID] = struct{}{} // DELETE
	b.wakeupPending = true                   // DELETE
	b.cond.Signal()
}
```
becomes:
```go
	b.cond.Signal()
}
```

- [ ] **Step 1.6: Delete `tryDeliverPriority` function entirely**

Remove the full function (lines 216–264 in current file):
```go
func (b *SpanBuffer) tryDeliverPriority(rHead, used int64) {
    ...
}
```

- [ ] **Step 1.7: Remove `tryDeliverPriority` call from `runSweeper()`**

Delete this line from `runSweeper()`:
```go
b.tryDeliverPriority(rHead, used)
```

- [ ] **Step 1.8: Run all buffer tests**

```bash
go test ./internal/buffer/... -v
```
Expected: all PASS, no compilation errors.

- [ ] **Step 1.9: Commit**

```bash
git add internal/buffer/buffer.go internal/buffer/buffer_test.go
git commit -m "refactor(buffer): drop out-of-order priority delivery"
```

---

## Task 2: Replace `chunkSize` with `pageSize` across all files

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go`
- Modify: `processor/retroactivesampling/internal/buffer/buffer_bench_test.go`
- Modify: `processor/retroactivesampling/config.go`
- Modify: `processor/retroactivesampling/factory.go`
- Modify: `processor/retroactivesampling/processor.go`
- Modify: `processor/retroactivesampling/processor_test.go`
- Modify: `processor/retroactivesampling/integration_test.go`

- [ ] **Step 2.1: Add `pageSize` package-level var and remove `chunkSize` from `SpanBuffer` struct**

In `buffer.go`, add after the `tombstoneID` declaration:
```go
var pageSize = os.Getpagesize()
```

In the `SpanBuffer` struct, delete the field:
```go
chunkSize    int
```

- [ ] **Step 2.2: Replace `New()` — remove `chunkSize` param, update validation and init**

New signature and body for `New()`:
```go
func New(
	file string,
	maxBytes int64,
	decisionWait time.Duration,
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
	maxBytes = (maxBytes / int64(pageSize)) * int64(pageSize)
	if maxBytes < 2*int64(pageSize) {
		return nil, fmt.Errorf("maxBytes must be >= 2*pageSize after rounding, got %d", maxBytes)
	}
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(maxBytes); err != nil {
		_ = f.Close()
		return nil, err
	}
	b := &SpanBuffer{
		f:               f,
		maxBytes:        maxBytes,
		decisionWait:    decisionWait,
		onMatch:         onMatch,
		evictObs:        evictObs,
		stage:           make([]byte, pageSize),
		scratch:         make([]byte, pageSize),
		interestEntries: make(map[pcommon.TraceID]*list.Element),
	}
	b.cond = sync.NewCond(&b.mu)
	b.wg.Add(1)
	go b.runSweeper()
	return b, nil
}
```

- [ ] **Step 2.3: Update `flushStage()` — replace `b.chunkSize` with `pageSize`**

In `flushStage()`, replace:
```go
b.wHead = (offset + int64(b.chunkSize)) % b.maxBytes
b.used += int64(b.chunkSize)
```
with:
```go
b.wHead = (offset + int64(pageSize)) % b.maxBytes
b.used += int64(pageSize)
```

- [ ] **Step 2.4: Update `Write()` — replace `b.chunkSize` with `pageSize`, update error message**

Replace the size-check error message and all `b.chunkSize` references:
```go
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	recSize := hdrSize + len(data)
	if recSize > pageSize {
		return fmt.Errorf("record size %d exceeds page size %d", recSize, pageSize)
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.stageN+recSize > pageSize {
		if err := b.flushStage(); err != nil {
			return err
		}
		b.mu.Lock()
		full := b.used == b.maxBytes
		b.mu.Unlock()
		if full {
			return ErrFull
		}
	}
	copy(b.stage[b.stageN:], traceID[:])
	binary.BigEndian.PutUint64(b.stage[b.stageN+16:], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(b.stage[b.stageN+24:], uint32(len(data)))
	copy(b.stage[b.stageN+28:], data)
	b.stageN += recSize
	return nil
}
```

- [ ] **Step 2.5: Update `runSweeper()` — replace all `b.chunkSize` with `pageSize`**

Five replacements inside `runSweeper()`:

| Old | New |
|-----|-----|
| `b.rHead = (b.rHead + int64(b.chunkSize)) % b.maxBytes` | `b.rHead = (b.rHead + int64(pageSize)) % b.maxBytes` |
| `b.used -= int64(b.chunkSize)` | `b.used -= int64(pageSize)` |
| `for pos+hdrSize <= b.chunkSize {` | `for pos+hdrSize <= pageSize {` |
| `if pos+recSize > b.chunkSize {` | `if pos+recSize > pageSize {` |
| `b.rHead = (rHead + int64(b.chunkSize)) % b.maxBytes` | `b.rHead = (rHead + int64(pageSize)) % b.maxBytes` |
| `b.used -= int64(b.chunkSize)` (post-loop) | `b.used -= int64(pageSize)` |

Also remove the `b.chunkSize` field reference in the `pressure` line — it doesn't reference chunkSize, so leave it unchanged.

- [ ] **Step 2.6: Update `newBuf` helper in `buffer_test.go`**

Replace the helper signature and body:
```go
func newBuf(t *testing.T, chunks int, wait time.Duration, onMatch func(pcommon.TraceID, []byte)) *buffer.SpanBuffer {
	t.Helper()
	buf, err := buffer.New(
		filepath.Join(t.TempDir(), "buf.ring"),
		int64(os.Getpagesize()*chunks),
		wait,
		onMatch,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}
```

Add `"os"` to imports in `buffer_test.go` (it uses `os.Getpagesize()`).

- [ ] **Step 2.7: Update `TestNew_validation` in `buffer_test.go`**

Replace the entire test:
```go
func TestNew_validation(t *testing.T) {
	dir := t.TempDir()
	ps := os.Getpagesize()

	t.Run("ok", func(t *testing.T) {
		buf, err := buffer.New(filepath.Join(dir, "ok.ring"), int64(ps*2), time.Second, nil, nil)
		require.NoError(t, err)
		require.NoError(t, buf.Close())
	})
	t.Run("decisionWait zero", func(t *testing.T) {
		_, err := buffer.New(filepath.Join(dir, "a.ring"), int64(ps*4), 0, nil, nil)
		require.Error(t, err)
	})
	t.Run("maxBytes rounds down below 2 pages", func(t *testing.T) {
		_, err := buffer.New(filepath.Join(dir, "b.ring"), int64(ps*2-1), time.Second, nil, nil)
		require.Error(t, err)
	})
}
```

- [ ] **Step 2.8: Update `TestWrite_recordTooLarge` in `buffer_test.go`**

```go
func TestWrite_recordTooLarge(t *testing.T) {
	ps := os.Getpagesize()
	buf := newBuf(t, 4, time.Hour, nil)
	data := make([]byte, ps) // hdrSize(28)+ps > ps
	err := buf.Write(traceID(), data, time.Now())
	require.ErrorContains(t, err, "exceeds page size")
}
```

- [ ] **Step 2.9: Update `TestWrite_fillsChunk` in `buffer_test.go`**

With `pageSize` records: use `data` sized so exactly 2 fit per chunk (`recSize = pageSize/2`), and the 3rd triggers a flush.

```go
func TestWrite_fillsChunk(t *testing.T) {
	// recSize = ps/2; 2 fit per chunk (2*(ps/2) = ps ≤ ps), 3rd triggers flush.
	ps := os.Getpagesize()
	buf := newBuf(t, 4, time.Hour, nil)
	data := make([]byte, ps/2-28) // recSize = ps/2
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
	// Third write: stageN + recSize > ps → flush chunk0, then write into new stage.
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
}
```

- [ ] **Step 2.10: Update `TestWrite_full` in `buffer_test.go`**

```go
func TestWrite_full(t *testing.T) {
	// 2-chunk ring. recSize = ps/2; 2 per chunk.
	// Writes 1–2: stageN=ps. Write 3: flush chunk0 (used=ps), stageN=ps/2.
	// Write 4: stageN=ps. Write 5: flush chunk1 (used=2ps=maxBytes) → ErrFull.
	ps := os.Getpagesize()
	buf := newBuf(t, 2, time.Hour, nil)
	data := make([]byte, ps/2-28) // recSize = ps/2
	for range 4 {
		require.NoError(t, buf.Write(traceID(), data, time.Now()))
	}
	err := buf.Write(traceID(), data, time.Now())
	require.ErrorIs(t, err, buffer.ErrFull)
}
```

- [ ] **Step 2.11: Update sweeper and interest tests — fix `newBuf` call sites**

Every call `newBuf(t, cs, N, ...)` drops the `cs` arg. The remaining call sites with explicit `buffer.New` (in `TestSweeper_delivery`, `TestSweeper_eviction`, `TestSweeper_pressure_eviction`, `TestSweeper_crossChunkDelivery` [already deleted], `TestClose_flushesStage`) replace their `buffer.New` calls:

Old pattern:
```go
buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(cs*4), cs, time.Hour, onMatch, nil)
```
New pattern (drop the `cs` arg, use `int64(os.Getpagesize()*4)` for maxBytes):
```go
buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(os.Getpagesize()*4), time.Hour, onMatch, nil)
```

Update `TestSweeper_pressure_eviction` to use `ps/2-28` data (recSize=ps/2, 2 per chunk, 8 records fill 4 chunks):
```go
func TestSweeper_pressure_eviction(t *testing.T) {
	ps := os.Getpagesize()
	var evictions int
	evictObs := func(time.Duration) { evictions++ }

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(ps*4), time.Hour, nil, evictObs)
	require.NoError(t, err)

	data := make([]byte, ps/2-28) // recSize = ps/2; 2 per chunk
	now := time.Now()
	// 8 writes fill 4 chunks: writes 1–2 in stage, write 3 flushes chunk0, etc.
	// Close flushes the final stage chunk; sweeper drains with closed=true → all evicted.
	for range 8 {
		require.NoError(t, buf.Write(traceID(), data, now))
	}
	require.NoError(t, buf.Close())

	require.Equal(t, 8, evictions)
}
```

- [ ] **Step 2.12: Update `buffer_bench_test.go`**

In `BenchmarkWrite`, remove `chunkSize` arg and update `buffer.New` call:
```go
func BenchmarkWrite(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceID(), ptrace.StatusCodeOk, 100))
	require.NoError(b, err)

	ps := os.Getpagesize()
	buf, err := buffer.New(filepath.Join(b.TempDir(), "buf.ring"), int64(ps*2), time.Hour, nil, nil)
	require.NoError(b, err)

	now := time.Now().Add(-time.Second)
	for range int64(ps * 2) {
		err := buf.Write(traceID(), data, now)
		if err == buffer.ErrFull {
			break
		}
		require.NoError(b, err)
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = buf.Write(traceID(), data, now)
	}
}
```

Add `"os"` to imports in `buffer_bench_test.go`.

- [ ] **Step 2.13: Update `config.go` — remove `ChunkSize` field**

Remove from `Config` struct:
```go
ChunkSize        int                     `mapstructure:"chunk_size"`
```

- [ ] **Step 2.14: Update `factory.go` — remove `ChunkSize` from default config**

Remove from `createDefaultConfig()`:
```go
ChunkSize:        4096,
```

- [ ] **Step 2.15: Update `processor.go` — drop `cfg.ChunkSize` from `buffer.New` call**

Replace the `buffer.New` call:
```go
buf, err := buffer.New(
	cfg.BufferFile,
	cfg.MaxBufferBytes,
	cfg.DecisionWaitTime,
	p.onMatch,
	func(d time.Duration) {
		tb.RetroactiveSamplingBufferSpanAgeOnEviction.Record(context.Background(), int64(d.Seconds()))
	},
)
```

- [ ] **Step 2.16: Update `processor_test.go` — remove `ChunkSize` from two `Config` literals**

Both `Config` structs (lines ~114 and ~300): delete the `ChunkSize: 4096,` line from each.

- [ ] **Step 2.17: Update `integration_test.go` — remove `ChunkSize` from `Config` literal**

In `newE2EProcessor`, delete `ChunkSize: 4096,` from the `cfg` literal.

- [ ] **Step 2.18: Run all buffer tests**

```bash
go test ./internal/buffer/... -v
```
Expected: all PASS.

- [ ] **Step 2.19: Run all processor tests**

```bash
go test ./... -v
```
Expected: all PASS (including processor unit tests and integration tests).

- [ ] **Step 2.20: Commit**

```bash
git add internal/buffer/buffer.go internal/buffer/buffer_test.go internal/buffer/buffer_bench_test.go \
    config.go factory.go processor.go processor_test.go integration_test.go
git commit -m "refactor(buffer): replace configurable chunkSize with os.Getpagesize()"
```

---

## Task 3: Large record write path

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 3.1: Write `TestWrite_largeRecord_full` (failing)**

Add to `buffer_test.go`:
```go
func TestWrite_largeRecord_full(t *testing.T) {
	// 4-chunk ring. Fill 2 chunks with small records (leaving 2 free).
	// A large record needing 3 chunks returns ErrFull.
	ps := os.Getpagesize()
	buf := newBuf(t, 4, time.Hour, nil)

	// 1 small record per chunk (recSize = ps-1, just under a page).
	smallData := make([]byte, ps-29) // hdrSize(28) + ps-29 = ps-1 < ps
	require.NoError(t, buf.Write(traceID(), smallData, time.Now()))
	require.NoError(t, buf.Write(traceID(), smallData, time.Now()))
	require.NoError(t, buf.Flush()) // used = 2*ps (2 chunks committed)

	// Large record: dataLen = 2*ps → numChunks = ceil((28 + 2*ps) / ps) = 3.
	// 3 chunks needed, only 2 free → ErrFull.
	largeData := make([]byte, 2*ps)
	err := buf.Write(traceID(), largeData, time.Now())
	require.ErrorIs(t, err, buffer.ErrFull)
}
```

- [ ] **Step 3.2: Run to confirm failure**

```bash
go test ./internal/buffer/... -v -run TestWrite_largeRecord_full
```
Expected: FAIL — `buf.Write` currently returns `"record size ... exceeds page size ..."`, not `ErrFull`.

- [ ] **Step 3.3: Delete `TestWrite_recordTooLarge` from `buffer_test.go`**

Large records are now supported, so remove the test that expects an error for them.

- [ ] **Step 3.4: Implement the large record write path in `Write()`**

Replace `Write()` entirely:

```go
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	if hdrSize+len(data) <= pageSize {
		// Small record: accumulate in stage, flush when full.
		recSize := hdrSize + len(data)
		if b.stageN+recSize > pageSize {
			if err := b.flushStage(); err != nil {
				return err
			}
			b.mu.Lock()
			full := b.used == b.maxBytes
			b.mu.Unlock()
			if full {
				return ErrFull
			}
		}
		copy(b.stage[b.stageN:], traceID[:])
		binary.BigEndian.PutUint64(b.stage[b.stageN+16:], uint64(insertedAt.UnixNano()))
		binary.BigEndian.PutUint32(b.stage[b.stageN+24:], uint32(len(data)))
		copy(b.stage[b.stageN+28:], data)
		b.stageN += recSize
		return nil
	}

	// Large record: must start at a chunk boundary.
	if b.stageN > 0 {
		if err := b.flushStage(); err != nil {
			return err
		}
	}
	numChunks := (hdrSize + len(data) + pageSize - 1) / pageSize
	b.mu.Lock()
	available := b.maxBytes - b.used
	b.mu.Unlock()
	if int64(numChunks)*int64(pageSize) > available {
		return ErrFull
	}

	// First chunk: header + first (pageSize-hdrSize) bytes of data.
	copy(b.stage[0:], traceID[:])
	binary.BigEndian.PutUint64(b.stage[16:], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(b.stage[24:], uint32(len(data)))
	n := copy(b.stage[hdrSize:], data) // copies exactly pageSize-hdrSize bytes
	b.stageN = hdrSize + n
	if err := b.flushStage(); err != nil {
		return err
	}

	// Continuation chunks: raw data, no header.
	off := n
	for off < len(data) {
		n = copy(b.stage[0:], data[off:]) // copies min(pageSize, remaining)
		b.stageN = n                      // flushStage zeros stage[n:pageSize]
		if err := b.flushStage(); err != nil {
			return err
		}
		off += n
	}
	return nil
}
```

- [ ] **Step 3.5: Run `TestWrite_largeRecord_full`**

```bash
go test ./internal/buffer/... -v -run TestWrite_largeRecord_full
```
Expected: PASS.

- [ ] **Step 3.6: Run all buffer tests**

```bash
go test ./internal/buffer/... -v
```
Expected: all PASS.

- [ ] **Step 3.7: Commit**

```bash
git add internal/buffer/buffer.go internal/buffer/buffer_test.go
git commit -m "feat(buffer): implement large record write path with ErrFull precheck"
```

---

## Task 4: Large record sweeper path

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 4.1: Write `TestWrite_largeRecord` (failing)**

```go
func TestWrite_largeRecord(t *testing.T) {
	// Write a 3-chunk record, add interest, verify full payload delivered.
	ps := os.Getpagesize()
	var mu sync.Mutex
	var matched [][]byte
	onMatch := func(_ pcommon.TraceID, payload []byte) {
		mu.Lock()
		matched = append(matched, payload)
		mu.Unlock()
	}

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(ps*5), time.Hour, onMatch, nil)
	require.NoError(t, err)

	id := traceID()
	data := make([]byte, 2*ps) // numChunks = 3
	for i := range data {
		data[i] = byte(i % 251) // non-zero pattern to verify correctness
	}
	require.NoError(t, buf.Write(id, data, time.Now()))
	buf.AddInterest(id)
	require.NoError(t, buf.Close())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, matched, 1)
	require.Equal(t, data, matched[0])
}
```

- [ ] **Step 4.2: Write `TestSweeper_largeRecordEviction` (failing)**

```go
func TestSweeper_largeRecordEviction(t *testing.T) {
	// Expired large record must call evictObs exactly once, not once per chunk.
	ps := os.Getpagesize()
	var evictions int
	evictObs := func(time.Duration) { evictions++ }

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(ps*5), 50*time.Millisecond, nil, evictObs)
	require.NoError(t, err)

	old := time.Now().Add(-time.Second) // well past decisionWait
	data := make([]byte, 2*ps)          // numChunks = 3
	require.NoError(t, buf.Write(traceID(), data, old))
	require.NoError(t, buf.Close())

	require.Equal(t, 1, evictions)
}
```

- [ ] **Step 4.3: Run to confirm both tests fail**

```bash
go test ./internal/buffer/... -v -run "TestWrite_largeRecord$|TestSweeper_largeRecordEviction"
```
Expected: both FAIL — the sweeper currently misreads continuation chunks as new records.

- [ ] **Step 4.4: Implement the multi-chunk sweeper path in `runSweeper()`**

Replace `runSweeper()` entirely:

```go
func (b *SpanBuffer) runSweeper() {
	defer b.wg.Done()
	b.mu.Lock()
	for {
		for b.used == 0 && !b.closed {
			b.cond.Wait()
		}
		if b.closed && b.used == 0 {
			b.mu.Unlock()
			return
		}
		rHead := b.rHead
		used := b.used
		b.mu.Unlock()

		chunksConsumed := 1
		fullyConsumed := true
		needsWriteback := false
		if _, err := b.f.ReadAt(b.scratch, rHead); err != nil {
			b.mu.Lock()
			b.rHead = (b.rHead + int64(pageSize)) % b.maxBytes
			b.used -= int64(pageSize)
			b.wakeupPending = true
			b.cond.Broadcast()
			continue
		}

		var minRemaining time.Duration
		pos := 0
		for pos+hdrSize <= pageSize {
			var tid pcommon.TraceID
			copy(tid[:], b.scratch[pos:pos+16])
			if tid == (pcommon.TraceID{}) {
				break
			}
			dataLen := int(binary.BigEndian.Uint32(b.scratch[pos+24:]))
			recSize := hdrSize + dataLen

			// Multi-chunk record: always at pos==0 (enforced by write path).
			if pos == 0 && recSize > pageSize {
				numChunks := (hdrSize + dataLen + pageSize - 1) / pageSize
				if tid == tombstoneID {
					// Already delivered/evicted: skip all N chunks.
					// dataLen is preserved after tombstoning (only [0:16] was overwritten).
					chunksConsumed = numChunks
					break
				}
				if int64(numChunks)*int64(pageSize) > used {
					// Not all chunks flushed yet: wait.
					fullyConsumed = false
					break
				}
				nsec := int64(binary.BigEndian.Uint64(b.scratch[pos+16:]))
				insertedAt := time.Unix(0, nsec)
				b.mu.Lock()
				interesting := b.hasInterestLocked(tid)
				pressure := b.used > b.maxBytes*3/4 || b.closed
				b.mu.Unlock()

				age := time.Since(insertedAt)
				if interesting || age >= b.decisionWait || pressure {
					payload := make([]byte, dataLen)
					copy(payload, b.scratch[hdrSize:pageSize]) // first chunk data
					payloadOff := pageSize - hdrSize
					for i := 1; i < numChunks; i++ {
						chunkOff := (rHead + int64(i)*int64(pageSize)) % b.maxBytes
						remaining := dataLen - payloadOff
						if remaining > pageSize {
							remaining = pageSize
						}
						_, _ = b.f.ReadAt(payload[payloadOff:payloadOff+remaining], chunkOff)
						payloadOff += remaining
					}
					if interesting {
						b.onMatch(tid, payload)
					} else {
						b.evictObs(age)
					}
					copy(b.scratch[0:], tombstoneID[:])
					needsWriteback = true
					chunksConsumed = numChunks
				} else {
					remaining := b.decisionWait - age
					if minRemaining == 0 || remaining < minRemaining {
						minRemaining = remaining
					}
					fullyConsumed = false
				}
				break // always exit pos loop after multi-chunk record
			}

			// Single-chunk record.
			if pos+recSize > pageSize {
				break
			}
			if tid == tombstoneID {
				pos += recSize
				continue
			}
			nsec := int64(binary.BigEndian.Uint64(b.scratch[pos+16:]))
			insertedAt := time.Unix(0, nsec)

			b.mu.Lock()
			interesting := b.hasInterestLocked(tid)
			pressure := b.used > b.maxBytes*3/4 || b.closed
			b.mu.Unlock()

			age := time.Since(insertedAt)
			if interesting {
				payload := make([]byte, dataLen)
				copy(payload, b.scratch[pos+hdrSize:pos+recSize])
				b.onMatch(tid, payload)
				copy(b.scratch[pos:], tombstoneID[:])
				needsWriteback = true
				pos += recSize
				continue
			}
			if age >= b.decisionWait || pressure {
				b.evictObs(age)
				copy(b.scratch[pos:], tombstoneID[:])
				needsWriteback = true
				pos += recSize
				continue
			}
			remaining := b.decisionWait - age
			if minRemaining == 0 || remaining < minRemaining {
				minRemaining = remaining
			}
			fullyConsumed = false
			pos += recSize
		}

		if !fullyConsumed {
			if needsWriteback {
				_, _ = b.f.WriteAt(b.scratch, rHead)
			}
			time.AfterFunc(minRemaining, func() {
				b.mu.Lock()
				b.wakeupPending = true
				b.cond.Signal()
				b.mu.Unlock()
			})
		}

		b.mu.Lock()
		if fullyConsumed {
			b.rHead = (rHead + int64(chunksConsumed)*int64(pageSize)) % b.maxBytes
			b.used -= int64(chunksConsumed) * int64(pageSize)
			b.pruneInterestLocked()
			b.wakeupPending = true
			b.cond.Broadcast()
		} else {
			if !b.wakeupPending {
				b.cond.Wait()
			}
			b.wakeupPending = false
		}
	}
}
```

- [ ] **Step 4.5: Run new tests**

```bash
go test ./internal/buffer/... -v -run "TestWrite_largeRecord$|TestSweeper_largeRecordEviction"
```
Expected: both PASS.

- [ ] **Step 4.6: Run all buffer tests**

```bash
go test ./internal/buffer/... -v
```
Expected: all PASS.

- [ ] **Step 4.7: Run all processor tests**

```bash
go test ./... -v
```
Expected: all PASS.

- [ ] **Step 4.8: Run validation script**

```bash
cd ../..  # workspace root
scripts/validate.sh -stop 60
```
Expected: `tracegen_spans_received_total{reason="non-error"}` = 0; majority of traces at `completeness="full"`.

- [ ] **Step 4.9: Commit**

```bash
git add internal/buffer/buffer.go internal/buffer/buffer_test.go
git commit -m "feat(buffer): implement multi-chunk sweeper path for large records"
```
