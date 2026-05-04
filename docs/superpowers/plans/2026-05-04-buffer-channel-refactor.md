# SpanBuffer Channel-Based Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace SpanBuffer's mutex/cond/WaitGroup coordination with two goroutines (writer + sweeper) communicating via channels and one shared atomic.

**Architecture:** Writer goroutine owns stage/wHead as goroutine-local state; receives writeReq from buffered writeCh. Sweeper owns rHead/interest state, wakes on wakeup channel, signals pageFreed after consuming pages. One shared `atomic.Int64` tracks ring buffer occupancy.

**Tech Stack:** Go stdlib — `sync/atomic`, channels, `sync.RWMutex` (interest set only), `time.AfterFunc`, `time.NewTicker`.

---

## File Map

| File | Action | What changes |
|------|--------|--------------|
| `processor/retroactivesampling/internal/buffer/buffer.go` | Modify | Full refactor: struct, New, Write, Close, AddInterest, HasInterest, runWriter, runSweeper, processPage; remove Flush |
| `processor/retroactivesampling/processor.go` | Modify | Drop `buf.Flush()` call in processTraces |
| `processor/retroactivesampling/internal/buffer/buffer_test.go` | Modify | Rewrite TestWrite_full and TestWrite_largeRecord_full; drop Flush() calls before Close() in 3 tests |

---

### Task 1: Adapt tests to new ErrFull semantics and remove Flush() calls

The two ErrFull tests pin exact ring-buffer-full semantics that no longer hold after the refactor.
Rewrite them before touching production code so a future red run is meaningful.

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 1: Rewrite TestWrite_full**

Replace the current test (lines 106–118) with:

```go
func TestWrite_full(t *testing.T) {
	// Write until ErrFull; must happen within pageCount*10 iterations.
	ps := os.Getpagesize()
	buf := newBuf(t, 2, time.Hour, nil)
	data := make([]byte, ps/2-28)
	const maxIter = 20 * 2 // 10× page count upper bound
	gotFull := false
	for range maxIter {
		if err := buf.Write(traceID(), data, time.Now()); errors.Is(err, buffer.ErrFull) {
			gotFull = true
			break
		}
	}
	require.True(t, gotFull, "expected ErrFull before iteration limit")
}
```

Add `"errors"` to the import block.

- [ ] **Step 2: Rewrite TestWrite_largeRecord_full**

Replace lines 76–93:

```go
func TestWrite_largeRecord_full(t *testing.T) {
	// 4-chunk ring. Fill 2 chunks; a 3-chunk large record must eventually return ErrFull.
	ps := os.Getpagesize()
	buf := newBuf(t, 4, time.Hour, nil)

	smallData := make([]byte, ps-29)
	require.NoError(t, buf.Write(traceID(), smallData, time.Now()))
	require.NoError(t, buf.Write(traceID(), smallData, time.Now()))

	// Write small records until the two stage-pages flush (ticker or write-path flush).
	// Then attempt large record; it needs 3 pages but only 2 remain.
	largeData := make([]byte, 2*ps)
	gotFull := false
	for range 40 {
		if err := buf.Write(traceID(), largeData, time.Now()); errors.Is(err, buffer.ErrFull) {
			gotFull = true
			break
		}
		time.Sleep(10 * time.Millisecond) // let ticker flush staged pages
	}
	require.True(t, gotFull, "expected ErrFull for large record exceeding free space")
}
```

- [ ] **Step 3: Drop Flush() calls from sweeper tests**

In `TestSweeper_delivery` (line 168): remove `require.NoError(t, buf.Flush())`.

In `TestSweeper_eviction` (line 187): remove `require.NoError(t, buf.Flush())`.

In `TestWrite_largeRecord_full` (old version, already replaced above): Flush call gone.

- [ ] **Step 4: Verify tests still compile and the two new ErrFull tests pass**

```bash
cd /Users/p.vernigorov/code/retroactivesampling
go test ./processor/retroactivesampling/internal/buffer/... -run 'TestWrite_full|TestWrite_largeRecord_full|TestSweeper_delivery|TestSweeper_eviction' -v -timeout 30s
```

Expected: TestWrite_full and TestWrite_largeRecord_full pass (buffer still has Flush, old ErrFull semantics still hold — these new versions are looser so they should still pass). TestSweeper_delivery and TestSweeper_eviction pass.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "test(buffer): adapt ErrFull tests and drop Flush() calls ahead of channel refactor"
```

---

### Task 2: Remove Flush() from processor.go

**Files:**
- Modify: `processor/retroactivesampling/processor.go`

- [ ] **Step 1: Remove the Flush call**

In `processTraces` (around line 119), delete:

```go
	if err := p.buf.Flush(); err != nil {
		p.logger.Error("buffer flush failed", zap.Error(err))
	}
```

- [ ] **Step 2: Build to verify**

```bash
go build ./processor/retroactivesampling/...
```

Expected: compiles (Flush() still exists in buffer.go at this point).

- [ ] **Step 3: Commit**

```bash
git add processor/retroactivesampling/processor.go
git commit -m "feat(processor): remove explicit Flush call — writer goroutine auto-flushes via ticker"
```

---

### Task 3: Replace struct fields and add writeReq type

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`

- [ ] **Step 1: Replace imports**

New import block:

```go
import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
)
```

- [ ] **Step 2: Add writeReq type after the var block**

```go
type writeReq struct {
	traceID    pcommon.TraceID
	data       []byte
	insertedAt time.Time
}
```

- [ ] **Step 3: Replace SpanBuffer struct**

Old struct (lines 35–60) → new:

```go
type SpanBuffer struct {
	f            *os.File
	maxBytes     int64
	decisionWait time.Duration
	onMatch      func(pcommon.TraceID, []byte)
	evictObs     func(time.Duration)

	writeCh    chan writeReq
	wakeup     chan struct{}
	pageFreed  chan struct{}
	writerDone chan struct{}
	closeCh    chan struct{}
	used       atomic.Int64

	interestMu      sync.RWMutex
	interestEntries map[pcommon.TraceID]*list.Element
	interestList    list.List

	wg sync.WaitGroup
}
```

- [ ] **Step 4: Build (will fail — methods reference removed fields)**

```bash
go build ./processor/retroactivesampling/internal/buffer/... 2>&1 | head -40
```

Expected: many errors referencing `b.mu`, `b.cond`, `b.stage`, `b.stageN`, etc. That's fine — confirms old field removals are real.

- [ ] **Step 5: Commit the struct skeleton**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "refactor(buffer): replace struct fields with channel-based actor model skeleton"
```

---

### Task 4: Rewrite New()

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`

- [ ] **Step 1: Replace the New() function body**

```go
func New(file string, maxBytes int64, decisionWait time.Duration, onMatch func(pcommon.TraceID, []byte), evictObs func(time.Duration)) (*SpanBuffer, error) {
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
	pageCount := int(maxBytes / int64(pageSize))
	b := &SpanBuffer{
		f:               f,
		maxBytes:        maxBytes,
		decisionWait:    decisionWait,
		onMatch:         onMatch,
		evictObs:        evictObs,
		writeCh:         make(chan writeReq, pageCount),
		wakeup:          make(chan struct{}, 1),
		pageFreed:       make(chan struct{}, 1),
		writerDone:      make(chan struct{}),
		closeCh:         make(chan struct{}),
		interestEntries: make(map[pcommon.TraceID]*list.Element),
	}
	b.wg.Add(2)
	go b.runWriter()
	go b.runSweeper()
	return b, nil
}
```

Also remove the now-unused `"sync/atomic"` import note — `atomic.Int64` is a struct value, no import needed beyond `sync/atomic`. Actually `atomic.Int64` is in `sync/atomic` — keep that import.

Wait: `atomic.Int64` is `sync/atomic.Int64`. The field `used atomic.Int64` references it as a type. The import block needs `"sync/atomic"`. But since `atomic.Int64` in the struct is a value type, we just need it imported. However in Go, if you use `sync/atomic` as a package reference you write `atomic.Int64`. Check that the import alias is just `"sync/atomic"` (not aliased).

- [ ] **Step 2: Build**

```bash
go build ./processor/retroactivesampling/internal/buffer/... 2>&1 | head -40
```

Expected: still many errors from unrewritten methods, but New() itself should be clean.

- [ ] **Step 3: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "refactor(buffer): rewrite New() for channel-based actor model"
```

---

### Task 5: Rewrite public API methods (Write, Close, AddInterest, HasInterest, helpers)

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`

- [ ] **Step 1: Replace Write()**

Delete the old Write() (lines 152–213) and flushStage() (lines 215–231). Replace with:

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

- [ ] **Step 2: Replace Close()**

Delete the old Close() (lines 401–414). Replace with:

```go
func (b *SpanBuffer) Close() error {
	close(b.closeCh)
	b.wg.Wait()
	return b.f.Close()
}
```

- [ ] **Step 3: Delete Flush()**

Delete the Flush() method (lines 391–398).

- [ ] **Step 4: Replace AddInterest()**

```go
func (b *SpanBuffer) AddInterest(traceID pcommon.TraceID) {
	b.interestMu.Lock()
	b.pruneInterestLocked()
	if el, ok := b.interestEntries[traceID]; ok {
		b.interestList.MoveToFront(el)
		el.Value.(*interestEntry).addedAt = time.Now()
	} else {
		el := b.interestList.PushFront(&interestEntry{id: traceID, addedAt: time.Now()})
		b.interestEntries[traceID] = el
	}
	b.interestMu.Unlock()
	select { case b.wakeup <- struct{}{}: default: }
}
```

- [ ] **Step 5: Replace HasInterest() and add hasInterestRLocked()**

Replace old HasInterest and hasInterestLocked:

```go
func (b *SpanBuffer) HasInterest(traceID pcommon.TraceID) bool {
	b.interestMu.RLock()
	defer b.interestMu.RUnlock()
	return b.hasInterestRLocked(traceID)
}

// hasInterestRLocked checks interest without any map/list mutation. Expired entries
// linger until the next AddInterest prunes them; correctness holds since this returns
// false for expired entries regardless.
func (b *SpanBuffer) hasInterestRLocked(traceID pcommon.TraceID) bool {
	el, ok := b.interestEntries[traceID]
	if !ok {
		return false
	}
	return time.Since(el.Value.(*interestEntry).addedAt) < b.decisionWait
}
```

- [ ] **Step 6: Update pruneInterestLocked() — no change needed**

The existing pruneInterestLocked() (lines 135–148) is already correct. Verify it still exists and uses `b.interestList` / `b.interestEntries` (no `b.mu` references).

- [ ] **Step 7: Build**

```bash
go build ./processor/retroactivesampling/internal/buffer/... 2>&1 | head -40
```

Expected: errors only from missing runWriter/runSweeper implementations (old methods removed).

- [ ] **Step 8: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "refactor(buffer): rewrite Write/Close/AddInterest/HasInterest for channel actor model"
```

---

### Task 6: Implement runWriter()

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`

- [ ] **Step 1: Delete the old runSweeper() body temporarily**

The old runSweeper() references `b.mu`, `b.cond`, etc. Comment it out or stub it to make the file compile while we work:

```go
func (b *SpanBuffer) runSweeper() {
	defer b.wg.Done()
	// TODO: implement
	<-b.writerDone
}
```

- [ ] **Step 2: Write runWriter()**

Replace the old, empty runSweeper stub or add new function:

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
				stageN = 0
				return nil
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
		if hdrSize+len(req.data) <= pageSize {
			recSize := hdrSize + len(req.data)
			if stageN+recSize > pageSize {
				_ = flush()
				if stageN != 0 { // flush returned early on closeCh
					return
				}
			}
			copy(stage[stageN:], req.traceID[:])
			binary.BigEndian.PutUint64(stage[stageN+16:], uint64(req.insertedAt.UnixNano()))
			binary.BigEndian.PutUint32(stage[stageN+24:], uint32(len(req.data)))
			copy(stage[stageN+28:], req.data)
			stageN += recSize
			return
		}

		// Large record: flush current stage first.
		if stageN > 0 {
			_ = flush()
			if stageN != 0 {
				return
			}
		}
		numChunks := (hdrSize + len(req.data) + pageSize - 1) / pageSize
		if int64(numChunks)*int64(pageSize) > b.maxBytes {
			return // unflushably large: drop silently
		}
		// Claim all N pages upfront.
		for b.used.Load()+int64(numChunks)*int64(pageSize) > b.maxBytes {
			select {
			case <-b.pageFreed:
			case <-b.closeCh:
				return
			}
		}
		// Write first chunk: header + first (pageSize-hdrSize) data bytes.
		copy(stage[0:], req.traceID[:])
		binary.BigEndian.PutUint64(stage[16:], uint64(req.insertedAt.UnixNano()))
		binary.BigEndian.PutUint32(stage[24:], uint32(len(req.data)))
		n := copy(stage[hdrSize:], req.data)
		clear(stage[hdrSize+n:])
		if _, err := b.f.WriteAt(stage, wHead); err != nil {
			return
		}
		wHead = (wHead + int64(pageSize)) % b.maxBytes

		// Write continuation chunks.
		off := n
		for off < len(req.data) {
			n = copy(stage[0:], req.data[off:])
			clear(stage[n:])
			if _, err := b.f.WriteAt(stage, wHead); err != nil {
				return
			}
			wHead = (wHead + int64(pageSize)) % b.maxBytes
			off += n
		}
		// Only after all N chunks written: report occupancy and wake sweeper.
		b.used.Add(int64(numChunks) * int64(pageSize))
		select { case b.wakeup <- struct{}{}: default: }
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
			for {
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

- [ ] **Step 3: Build**

```bash
go build ./processor/retroactivesampling/internal/buffer/... 2>&1 | head -40
```

Expected: compiles (runSweeper is stubbed).

- [ ] **Step 4: Run interest and basic tests with stub sweeper**

```bash
go test ./processor/retroactivesampling/internal/buffer/... -run 'TestNew|TestInterest|TestWrite_full|TestWrite_largeRecord_full' -v -timeout 30s
```

Expected: TestNew_validation passes. TestInterest tests pass (interest methods are pure in-memory). TestWrite_full and TestWrite_largeRecord_full: ErrFull may not appear with stub sweeper (no pageFreed signals), but they should not hang — write loop hits writeCh capacity and returns ErrFull.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "feat(buffer): implement runWriter goroutine with flush, large-record, and ticker paths"
```

---

### Task 7: Extract processPage() and implement runSweeper()

This task extracts the page-scanning logic from old runSweeper into a `processPage` method, then implements the new runSweeper.

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go`

- [ ] **Step 1: Write processPage()**

`processPage` takes the current scratch buffer contents (one page already read), rHead (for multi-chunk continuation reads), used (current occupancy snapshot), and closing flag. Returns: fullyConsumed bool, chunksConsumed int, minRemaining time.Duration. It also performs tombstone writebacks internally.

```go
func (b *SpanBuffer) processPage(scratch []byte, rHead int64, closing bool) (fullyConsumed bool, chunksConsumed int, minRemaining time.Duration) {
	used := b.used.Load()
	fullyConsumed = true
	chunksConsumed = 1
	needsWriteback := false

	pressure := used > b.maxBytes*3/4 || closing

	var pos int
	for pos+hdrSize <= pageSize {
		var tid pcommon.TraceID
		copy(tid[:], scratch[pos:pos+16])
		if tid == (pcommon.TraceID{}) {
			break
		}
		dataLen := int(binary.BigEndian.Uint32(scratch[pos+24:]))
		recSize := hdrSize + dataLen

		if pos == 0 && recSize > pageSize {
			numChunks := (hdrSize + dataLen + pageSize - 1) / pageSize
			if tid == tombstoneID {
				chunksConsumed = numChunks
				break
			}
			if int64(numChunks)*int64(pageSize) > used {
				fullyConsumed = false
				break
			}
			nsec := int64(binary.BigEndian.Uint64(scratch[pos+16:]))
			insertedAt := time.Unix(0, nsec)

			b.interestMu.RLock()
			interesting := b.hasInterestRLocked(tid)
			b.interestMu.RUnlock()

			age := time.Since(insertedAt)
			if interesting || age >= b.decisionWait || pressure {
				payload := make([]byte, dataLen)
				copy(payload, scratch[hdrSize:pageSize])
				payloadOff := pageSize - hdrSize
				for i := 1; i < numChunks; i++ {
					chunkOff := (rHead + int64(i)*int64(pageSize)) % b.maxBytes
					remaining := min(dataLen-payloadOff, pageSize)
					_, _ = b.f.ReadAt(payload[payloadOff:payloadOff+remaining], chunkOff)
					payloadOff += remaining
				}
				if interesting {
					b.onMatch(tid, payload)
				} else {
					b.evictObs(age)
				}
				copy(scratch[0:], tombstoneID[:])
				needsWriteback = true
				chunksConsumed = numChunks
			} else {
				remaining := b.decisionWait - age
				if minRemaining == 0 || remaining < minRemaining {
					minRemaining = remaining
				}
				fullyConsumed = false
			}
			break
		}

		if pos+recSize > pageSize {
			break
		}
		if tid == tombstoneID {
			pos += recSize
			continue
		}
		nsec := int64(binary.BigEndian.Uint64(scratch[pos+16:]))
		insertedAt := time.Unix(0, nsec)

		b.interestMu.RLock()
		interesting := b.hasInterestRLocked(tid)
		b.interestMu.RUnlock()

		age := time.Since(insertedAt)
		if interesting {
			payload := make([]byte, dataLen)
			copy(payload, scratch[pos+hdrSize:pos+recSize])
			b.onMatch(tid, payload)
			copy(scratch[pos:], tombstoneID[:])
			needsWriteback = true
			pos += recSize
			continue
		}
		if age >= b.decisionWait || pressure {
			b.evictObs(age)
			copy(scratch[pos:], tombstoneID[:])
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

	if needsWriteback {
		_, _ = b.f.WriteAt(scratch, rHead)
	}
	return fullyConsumed, chunksConsumed, minRemaining
}
```

- [ ] **Step 2: Replace the stub runSweeper() with the real implementation**

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

		if _, err := b.f.ReadAt(scratch, rHead); err != nil {
			// Unreadable page: skip it.
			rHead = (rHead + int64(pageSize)) % b.maxBytes
			b.used.Add(-int64(pageSize))
			select { case b.pageFreed <- struct{}{}: default: }
			continue
		}

		fullyConsumed, chunksConsumed, minRemaining := b.processPage(scratch, rHead, closing)

		if fullyConsumed {
			rHead = (rHead + int64(chunksConsumed)*int64(pageSize)) % b.maxBytes
			b.used.Add(-int64(chunksConsumed) * int64(pageSize))
			select { case b.pageFreed <- struct{}{}: default: }
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

- [ ] **Step 3: Build**

```bash
go build ./processor/retroactivesampling/internal/buffer/...
```

Expected: clean build.

- [ ] **Step 4: Run all buffer tests**

```bash
go test ./processor/retroactivesampling/internal/buffer/... -v -timeout 60s -count=1
```

Expected: all tests pass. Pay attention to:
- `TestSweeper_delivery` — delivery via onMatch
- `TestSweeper_eviction` — evictObs called once
- `TestSweeper_pressure_eviction` — all 8 records evicted
- `TestWrite_largeRecord` — full payload reassembled
- `TestSweeper_largeRecordEviction` — evictObs called once not per chunk
- `TestClose_flushesStage` — close triggers stage flush via ticker/drain path
- `TestWrite_full`, `TestWrite_largeRecord_full` — ErrFull within iteration bound

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "feat(buffer): implement processPage() and runSweeper() for channel actor model"
```

---

### Task 8: Final cleanup and full test run

**Files:**
- Modify: `processor/retroactivesampling/internal/buffer/buffer.go` (remove unused import if any)

- [ ] **Step 1: Check for unused imports or dead code**

```bash
go vet ./processor/retroactivesampling/internal/buffer/...
```

Fix any issues reported.

- [ ] **Step 2: Run full test suite including processor**

```bash
go test ./processor/retroactivesampling/... -v -timeout 60s -count=1
```

Expected: all tests pass.

- [ ] **Step 3: Run benchmark to confirm no regression**

```bash
go test ./processor/retroactivesampling/internal/buffer/... -bench=BenchmarkWrite -benchtime=3s -count=1
```

Note the result for comparison (no regression gate, just sanity check).

- [ ] **Step 4: Run gopls diagnostics**

```bash
# use gopls MCP go_diagnostics tool
```

Expected: no errors.

- [ ] **Step 5: Build entire module**

```bash
go build ./...
```

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer.go
git commit -m "chore(buffer): cleanup after channel-based actor model refactor"
```

---

## Notes

- `TestClose_flushesStage`: passes because the writer drain loop (in `closeCh` case) calls `flush()` after draining all pending writes — the staged record gets committed before `writerDone` closes.
- `TestSweeper_pressure_eviction`: relies on `closing = true` path in sweeper to force-evict all records. Close() → closeCh → writer drains + closes writerDone → sweeper sees writerDone, sets closing=true → all records evicted.
- The `errors` import added in Task 1 is needed for `errors.Is`. Verify it's in the test file's import block.
- `atomic.Int64` is a value type in `sync/atomic` package — field declaration `used atomic.Int64` needs import alias `"sync/atomic"` with package name `atomic`.
