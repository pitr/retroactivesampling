# Ring Buffer Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the file-per-trace buffer with a single pre-allocated ring buffer file that bounds disk usage to exactly `maxBytes`, with near-instant startup via a checkpoint file.

**Architecture:** A pre-allocated ring file (`buffer.ring`) holds variable-length delta records written sequentially with wrap-around. An in-memory index maps traceID→[]deltaRecord (offset+size). On `Close`, the index is serialized to `buffer.checkpoint`; on `New`, the checkpoint is loaded for instant restart. Disk = exactly `maxBytes`, always.

**Tech Stack:** Go stdlib only. `go.opentelemetry.io/collector/pdata/ptrace` for marshaling. `encoding/binary`, `bytes`, `os`, `sync`.

---

## File Map

| File | Change |
|---|---|
| `processor/retroactivesampling/internal/buffer/buffer.go` | Full rewrite |
| `processor/retroactivesampling/internal/buffer/buffer_test.go` | Full rewrite |
| `processor/retroactivesampling/processor.go` | Remove `onEvict` arg from `buffer.New` call |
| `processor/retroactivesampling/integration_test.go` | Add `MaxBufferBytes` to config |

Run all unit tests with:
```
cd processor/retroactivesampling && go test ./internal/buffer/... -v
```

Run integration tests (requires `-tags integration`):
```
cd processor/retroactivesampling && go test -tags integration -v -timeout 30s
```

---

## Record Format Reference

```
Ring record:  [traceID:32][insertedAt:8][dataLen:4][data:dataLen]
              total header = 44 bytes (hdrSize)

Skip record:  traceID = all-zeros, insertedAt = 0, dataLen = remaining-44
              sentinel for "jump readHead to 0"
```

---

## Task 1: Rewrite buffer.go skeleton (New + Close stub)

**Files:**
- Rewrite: `processor/retroactivesampling/internal/buffer/buffer.go`
- Rewrite: `processor/retroactivesampling/internal/buffer/buffer_test.go`

- [ ] **Step 1: Write the failing test**

Replace the entire content of `buffer_test.go`:

```go
package buffer_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

const (
	traceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	traceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	traceC = "cccccccccccccccccccccccccccccccc"
)

func singleSpanTraces(traceIDStr string, statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	var tid [16]byte
	copy(tid[:], traceIDStr)
	span.SetTraceID(pcommon.TraceID(tid))
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

// recSize returns the exact ring record size (header + proto) for a Traces value.
func recSize(t *testing.T, spans ptrace.Traces) int64 {
	t.Helper()
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(spans)
	require.NoError(t, err)
	return int64(44 + len(data))
}

func newBuf(t *testing.T) *buffer.SpanBuffer {
	return newBufSize(t, 1<<20)
}

func newBufSize(t *testing.T, maxBytes int64) *buffer.SpanBuffer {
	t.Helper()
	buf, err := buffer.New(t.TempDir(), maxBytes)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func TestNewRejectsZeroMaxBytes(t *testing.T) {
	_, err := buffer.New(t.TempDir(), 0)
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run test to see it fail**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run TestNewRejectsZeroMaxBytes -v
```

Expected: compile error (package `buffer` doesn't match new API yet).

- [ ] **Step 3: Replace buffer.go with skeleton**

Replace entire `buffer/buffer.go`:

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

	"go.opentelemetry.io/collector/pdata/ptrace"
)

const hdrSize = 44 // traceID(32) + insertedAt(8) + dataLen(4)

var zeroID [32]byte

const (
	cpMagic   = uint32(0x52494E47) // "RING"
	cpVersion = uint32(1)
)

type deltaRecord struct {
	offset int64
	size   int32
}

type SpanBuffer struct {
	dir      string
	maxBytes int64
	f        *os.File
	wHead    int64
	rHead    int64
	used     int64
	entries  map[string][]deltaRecord
	mu       sync.Mutex
}

func New(dir string, maxBytes int64) (*SpanBuffer, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maxBytes must be positive, got %d", maxBytes)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	b := &SpanBuffer{dir: dir, maxBytes: maxBytes, entries: make(map[string][]deltaRecord)}
	if err := b.tryLoadCheckpoint(); err == nil {
		return b, nil
	}
	f, err := os.OpenFile(b.ringPath(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(maxBytes); err != nil {
		f.Close()
		return nil, err
	}
	b.f = f
	return b, nil
}

func (b *SpanBuffer) ringPath() string { return filepath.Join(b.dir, "buffer.ring") }
func (b *SpanBuffer) cpPath() string   { return filepath.Join(b.dir, "buffer.checkpoint") }

func (b *SpanBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.saveCP(); err != nil {
		b.f.Close()
		return err
	}
	return b.f.Close()
}

func (b *SpanBuffer) WriteWithEviction(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
	return fmt.Errorf("not implemented")
}

func (b *SpanBuffer) Read(traceID string) (ptrace.Traces, bool, error) {
	return ptrace.Traces{}, false, fmt.Errorf("not implemented")
}

func (b *SpanBuffer) Delete(traceID string) error {
	return fmt.Errorf("not implemented")
}

func (b *SpanBuffer) saveCP() error   { return nil } // stub
func (b *SpanBuffer) tryLoadCheckpoint() error { return fmt.Errorf("no checkpoint") } // stub
```

- [ ] **Step 4: Run test**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run TestNewRejectsZeroMaxBytes -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/
git commit -m "feat(buffer): ring buffer skeleton with New/Close"
```

---

## Task 2: WriteWithEviction (no eviction, no wrap) + Read

- [ ] **Step 1: Add tests**

Append to `buffer_test.go`:

```go
func TestWriteAndRead(t *testing.T) {
	buf := newBuf(t)
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)

	require.NoError(t, buf.WriteWithEviction(traceA, tr, time.Now()))

	got, ok, err := buf.Read(traceA)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestWriteAppendsSpans(t *testing.T) {
	buf := newBuf(t)
	t1 := singleSpanTraces(traceA, ptrace.StatusCodeOk, 50)
	t2 := singleSpanTraces(traceA, ptrace.StatusCodeOk, 60)

	require.NoError(t, buf.WriteWithEviction(traceA, t1, time.Now()))
	require.NoError(t, buf.WriteWithEviction(traceA, t2, time.Now()))

	got, ok, err := buf.Read(traceA)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.SpanCount())
}

func TestReadMissingTrace(t *testing.T) {
	buf := newBuf(t)
	_, ok, err := buf.Read(traceA)
	require.NoError(t, err)
	assert.False(t, ok)
}
```

- [ ] **Step 2: Run tests to see them fail**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestWriteAndRead|TestWriteAppends|TestReadMissing" -v
```

Expected: FAIL — `WriteWithEviction` returns "not implemented".

- [ ] **Step 3: Implement WriteWithEviction and Read**

Replace the stub `WriteWithEviction` and `Read` in `buffer.go`:

```go
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

	var hdr [hdrSize]byte
	copy(hdr[:32], traceID)
	binary.BigEndian.PutUint64(hdr[32:40], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(hdr[40:44], uint32(len(delta)))

	offset := b.wHead
	if _, err := b.f.WriteAt(hdr[:], offset); err != nil {
		return err
	}
	if len(delta) > 0 {
		if _, err := b.f.WriteAt(delta, offset+hdrSize); err != nil {
			return err
		}
	}

	b.entries[traceID] = append(b.entries[traceID], deltaRecord{offset, int32(len(delta))})
	b.wHead += recSize
	b.used += recSize
	return nil
}

func (b *SpanBuffer) wrapLocked() {
	remaining := b.maxBytes - b.wHead
	if remaining >= hdrSize {
		var hdr [hdrSize]byte // all-zero traceID = skip record
		binary.BigEndian.PutUint32(hdr[40:44], uint32(remaining-hdrSize))
		_, _ = b.f.WriteAt(hdr[:], b.wHead)
	}
	b.used += remaining
	b.wHead = 0
}

func (b *SpanBuffer) sweepOneLocked() error {
	if b.rHead >= b.maxBytes {
		b.rHead = 0
	}
	// Fewer than hdrSize bytes remain at end: no skip record was written, just advance.
	if b.rHead+hdrSize > b.maxBytes {
		remaining := b.maxBytes - b.rHead
		b.used -= remaining
		b.rHead = 0
		return nil
	}

	var hdr [hdrSize]byte
	if _, err := b.f.ReadAt(hdr[:], b.rHead); err != nil {
		return err
	}
	dataLen := int64(binary.BigEndian.Uint32(hdr[40:44]))
	recSize := int64(hdrSize) + dataLen

	if bytes.Equal(hdr[:32], zeroID[:]) {
		// Skip record: used was incremented by (maxBytes - wHead) during wrapLocked,
		// which equals recSize = hdrSize + dataLen.
		b.used -= recSize
		b.rHead = 0
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
	return nil
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
		if _, err := b.f.ReadAt(buf, d.offset+hdrSize); err != nil {
			return ptrace.Traces{}, false, err
		}
		t, err := u.UnmarshalTraces(buf)
		if err != nil {
			return ptrace.Traces{}, false, err
		}
		t.ResourceSpans().MoveAndAppendTo(result.ResourceSpans())
	}
	return result, true, nil
}
```

- [ ] **Step 4: Run tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestWriteAndRead|TestWriteAppends|TestReadMissing" -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/
git commit -m "feat(buffer): implement WriteWithEviction and Read"
```

---

## Task 3: Eviction (sweep oldest delta)

- [ ] **Step 1: Add test**

Append to `buffer_test.go`:

```go
func TestEviction(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// maxBytes = rs: ring holds exactly one record.
	buf := newBufSize(t, rs)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), now))
	require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), now.Add(time.Millisecond)))

	_, ok, _ := buf.Read(traceA)
	assert.False(t, ok, "traceA should be evicted")
	_, ok, _ = buf.Read(traceB)
	assert.True(t, ok, "traceB should be present")
}

func TestPartialDeltaEviction(t *testing.T) {
	// Write two deltas for traceA; then write traceB which evicts only the first delta.
	// traceA should still be readable (from its second delta).
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// maxBytes = 3*rs: holds 3 records. Write A, A(again), B → first delta of A swept to make room.
	buf := newBufSize(t, 3*rs)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), now))
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 200), now.Add(time.Millisecond)))
	// Ring is full (2 records). Writing traceB needs space: sweep first delta of A.
	require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), now.Add(2*time.Millisecond)))

	// traceA still has its second delta (the 200ms span)
	got, ok, err := buf.Read(traceA)
	require.NoError(t, err)
	require.True(t, ok, "traceA should still be readable via its second delta")
	assert.Equal(t, 1, got.SpanCount())

	got, ok, err = buf.Read(traceB)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}
```

- [ ] **Step 2: Run tests to see them fail**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestEviction|TestPartialDelta" -v
```

Expected: FAIL — eviction not yet implemented (sweepOneLocked and wrapLocked stubs above are already in buffer.go from Task 2, but the eviction loop in WriteWithEviction needs both).

Actually, `sweepOneLocked` and `wrapLocked` were already added in Task 2. Run to verify:

```
cd processor/retroactivesampling && go test ./internal/buffer/... -v
```

Expected: `TestEviction` and `TestPartialDeltaEviction` should PASS (the implementation is already in place from Task 2). If they pass, skip Step 3.

- [ ] **Step 3: If tests fail, debug sweepOneLocked**

Check that `wrapLocked` and `sweepOneLocked` are present in `buffer.go` exactly as written in Task 2 Step 3. The eviction loop (`for b.used+recSize > b.maxBytes`) in `WriteWithEviction` drives both.

- [ ] **Step 4: Run all buffer tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "test(buffer): add eviction and partial-delta tests"
```

---

## Task 4: Ring wrap

- [ ] **Step 1: Add test**

Append to `buffer_test.go`:

```go
func TestWrap(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// maxBytes = 2*rs+5: holds 2 records, leaves 5 bytes at end (< hdrSize=44).
	// Third write triggers wrapLocked + sweep.
	buf := newBufSize(t, 2*rs+5)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), now))
	require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), now.Add(time.Millisecond)))
	require.NoError(t, buf.WriteWithEviction(traceC, singleSpanTraces(traceC, ptrace.StatusCodeOk, 100), now.Add(2*time.Millisecond)))

	_, ok, _ := buf.Read(traceA)
	assert.False(t, ok, "traceA should be evicted after wrap")

	got, ok, err := buf.Read(traceB)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())

	got, ok, err = buf.Read(traceC)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestWrapWithSkipRecord(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// maxBytes = 2*rs+50: 50 bytes at end (≥ hdrSize=44), so wrapLocked writes a skip record.
	buf := newBufSize(t, 2*rs+50)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), now))
	require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), now.Add(time.Millisecond)))
	require.NoError(t, buf.WriteWithEviction(traceC, singleSpanTraces(traceC, ptrace.StatusCodeOk, 100), now.Add(2*time.Millisecond)))

	_, ok, _ := buf.Read(traceA)
	assert.False(t, ok, "traceA should be evicted after wrap with skip record")

	got, ok, err := buf.Read(traceC)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}
```

- [ ] **Step 2: Run tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestWrap" -v
```

Expected: PASS (wrap logic is in `wrapLocked` and `sweepOneLocked` from Task 2).

- [ ] **Step 3: Run all buffer tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -v
```

Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/buffer_test.go
git commit -m "test(buffer): add wrap and skip-record tests"
```

---

## Task 5: Delete

- [ ] **Step 1: Add tests**

Append to `buffer_test.go`:

```go
func TestDelete(t *testing.T) {
	buf := newBuf(t)
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)

	require.NoError(t, buf.WriteWithEviction(traceA, tr, time.Now()))
	require.NoError(t, buf.Delete(traceA))

	_, ok, err := buf.Read(traceA)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDeleteNonExistent(t *testing.T) {
	buf := newBuf(t)
	assert.NoError(t, buf.Delete(traceA))
}

func TestDeleteOrphanSweptOnNextWrite(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	buf := newBufSize(t, rs)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), now))
	require.NoError(t, buf.Delete(traceA)) // orphan record in ring

	// Ring used=rs. Writing traceB triggers sweep of orphaned traceA record — no eviction callback,
	// just advances rHead past it.
	require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), now.Add(time.Millisecond)))

	_, ok, _ := buf.Read(traceA)
	assert.False(t, ok)
	_, ok, _ = buf.Read(traceB)
	assert.True(t, ok)
}
```

- [ ] **Step 2: Run tests to see them fail**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestDelete" -v
```

Expected: FAIL — `Delete` returns "not implemented".

- [ ] **Step 3: Implement Delete**

Replace the stub `Delete` in `buffer.go`:

```go
func (b *SpanBuffer) Delete(traceID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, traceID)
	return nil
}
```

- [ ] **Step 4: Run tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestDelete" -v
```

Expected: all PASS.

- [ ] **Step 5: Run all buffer tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -v
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/
git commit -m "feat(buffer): implement Delete"
```

---

## Task 6: Checkpoint (save + load)

- [ ] **Step 1: Add test**

Append to `buffer_test.go`:

```go
func TestCheckpointRoundtrip(t *testing.T) {
	dir := t.TempDir()

	// Phase 1: write data and close (triggers checkpoint save).
	{
		buf, err := buffer.New(dir, 1<<20)
		require.NoError(t, err)
		require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), time.Now()))
		require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), time.Now()))
		require.NoError(t, buf.Close())
	}

	// Phase 2: reopen — should load from checkpoint, not start fresh.
	{
		buf, err := buffer.New(dir, 1<<20)
		require.NoError(t, err)
		defer buf.Close()

		got, ok, err := buf.Read(traceA)
		require.NoError(t, err)
		require.True(t, ok, "traceA should survive Close+New via checkpoint")
		assert.Equal(t, 1, got.SpanCount())

		got, ok, err = buf.Read(traceB)
		require.NoError(t, err)
		require.True(t, ok, "traceB should survive Close+New via checkpoint")
		assert.Equal(t, 1, got.SpanCount())
	}
}

func TestCheckpointFreshStartOnCrash(t *testing.T) {
	dir := t.TempDir()

	// Write data but do NOT call Close — simulates crash.
	buf, err := buffer.New(dir, 1<<20)
	require.NoError(t, err)
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), time.Now()))
	// Intentionally skip buf.Close().

	// Reopen — no checkpoint exists, so starts fresh.
	buf2, err := buffer.New(dir, 1<<20)
	require.NoError(t, err)
	defer buf2.Close()

	_, ok, _ := buf2.Read(traceA)
	assert.False(t, ok, "after crash (no checkpoint), buffer starts fresh")
}

func TestCheckpointMaxBytesMismatch(t *testing.T) {
	dir := t.TempDir()

	buf, err := buffer.New(dir, 1<<20)
	require.NoError(t, err)
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), time.Now()))
	require.NoError(t, buf.Close())

	// Reopen with different maxBytes — checkpoint must be ignored, fresh start.
	buf2, err := buffer.New(dir, 2<<20)
	require.NoError(t, err)
	defer buf2.Close()

	_, ok, _ := buf2.Read(traceA)
	assert.False(t, ok, "maxBytes mismatch should cause fresh start")
}
```

- [ ] **Step 2: Run tests to see them fail**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestCheckpoint" -v
```

Expected: `TestCheckpointRoundtrip` FAIL (Close saves nothing, reopen starts fresh). Others may PASS by coincidence.

- [ ] **Step 3: Implement saveCP and tryLoadCheckpoint**

Replace the two stub functions in `buffer.go`:

```go
func (b *SpanBuffer) saveCP() error {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, cpMagic)
	binary.Write(&buf, binary.BigEndian, cpVersion)
	binary.Write(&buf, binary.BigEndian, b.maxBytes)
	binary.Write(&buf, binary.BigEndian, b.wHead)
	binary.Write(&buf, binary.BigEndian, b.rHead)
	binary.Write(&buf, binary.BigEndian, b.used)
	binary.Write(&buf, binary.BigEndian, uint32(len(b.entries)))
	for id, deltas := range b.entries {
		buf.WriteByte(byte(len(id)))
		buf.WriteString(id)
		binary.Write(&buf, binary.BigEndian, uint32(len(deltas)))
		for _, d := range deltas {
			binary.Write(&buf, binary.BigEndian, d.offset)
			binary.Write(&buf, binary.BigEndian, d.size)
		}
	}
	return os.WriteFile(b.cpPath(), buf.Bytes(), 0600)
}

func (b *SpanBuffer) tryLoadCheckpoint() error {
	data, err := os.ReadFile(b.cpPath())
	if err != nil {
		return err
	}
	r := bytes.NewReader(data)

	var mg, ver uint32
	if binary.Read(r, binary.BigEndian, &mg) != nil || mg != cpMagic {
		return fmt.Errorf("bad magic")
	}
	if binary.Read(r, binary.BigEndian, &ver) != nil || ver != cpVersion {
		return fmt.Errorf("bad version")
	}

	var maxBytes, wHead, rHead, used int64
	binary.Read(r, binary.BigEndian, &maxBytes)
	if maxBytes != b.maxBytes {
		return fmt.Errorf("maxBytes mismatch: checkpoint=%d, configured=%d", maxBytes, b.maxBytes)
	}
	binary.Read(r, binary.BigEndian, &wHead)
	binary.Read(r, binary.BigEndian, &rHead)
	binary.Read(r, binary.BigEndian, &used)

	var entryCount uint32
	binary.Read(r, binary.BigEndian, &entryCount)

	entries := make(map[string][]deltaRecord, entryCount)
	for i := uint32(0); i < entryCount; i++ {
		var idLen uint8
		binary.Read(r, binary.BigEndian, &idLen)
		idBytes := make([]byte, idLen)
		r.Read(idBytes)
		var deltaCount uint32
		binary.Read(r, binary.BigEndian, &deltaCount)
		deltas := make([]deltaRecord, deltaCount)
		for j := uint32(0); j < deltaCount; j++ {
			binary.Read(r, binary.BigEndian, &deltas[j].offset)
			binary.Read(r, binary.BigEndian, &deltas[j].size)
		}
		entries[string(idBytes)] = deltas
	}

	f, err := os.OpenFile(b.ringPath(), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if info.Size() != b.maxBytes {
		f.Close()
		return fmt.Errorf("ring file size mismatch: got %d, want %d", info.Size(), b.maxBytes)
	}

	b.f = f
	b.wHead, b.rHead, b.used = wHead, rHead, used
	b.entries = entries
	_ = os.Remove(b.cpPath())
	return nil
}
```

- [ ] **Step 4: Run tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -run "TestCheckpoint" -v
```

Expected: all PASS.

- [ ] **Step 5: Run all buffer tests**

```
cd processor/retroactivesampling && go test ./internal/buffer/... -v
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/internal/buffer/
git commit -m "feat(buffer): checkpoint save and load for instant restart"
```

---

## Task 7: Update processor.go and integration test

**Files:**
- Modify: `processor/retroactivesampling/processor.go`
- Modify: `processor/retroactivesampling/integration_test.go`

- [ ] **Step 1: Update processor.go**

In `processor/retroactivesampling/processor.go`, locate the `buffer.New` call in `newProcessor`:

```go
// BEFORE:
buf, err := buffer.New(cfg.BufferDir, cfg.MaxBufferBytes, func(traceID string, insertedAt time.Time) {
    ic.Delete(traceID)
    retentionHist.Record(context.Background(), time.Since(insertedAt).Seconds())
})
```

Replace with:

```go
// AFTER:
buf, err := buffer.New(cfg.BufferDir, cfg.MaxBufferBytes)
```

Remove the now-unused `retentionHist` variable and its meter call. The full updated `newProcessor` function:

```go
func newProcessor(set component.TelemetrySettings, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	ic := cache.New(cfg.MaxInterestCacheEntries)
	buf, err := buffer.New(cfg.BufferDir, cfg.MaxBufferBytes)
	if err != nil {
		return nil, err
	}
	chain, err := evaluator.Build(cfg.Rules)
	if err != nil {
		buf.Close()
		return nil, err
	}
	p := &retroactiveProcessor{
		logger: set.Logger,
		next:   next,
		buf:    buf,
		ic:     ic,
		eval:   chain,
	}
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision, set.Logger)
	return p, nil
}
```

- [ ] **Step 2: Update integration_test.go**

In `integration_test.go`, the `newE2EProcessor` function has a Config without `MaxBufferBytes`. Since `maxBytes = 0` now errors, add it:

```go
cfg := &proc.Config{
    BufferDir:               t.TempDir(),
    MaxBufferBytes:          100 << 20, // 100 MB
    MaxInterestCacheEntries: 1000,
    CoordinatorEndpoint:     coordAddr,
    Rules:                   []evaluator.RuleConfig{{Type: "error_status"}},
}
```

- [ ] **Step 3: Verify compile**

```
cd processor/retroactivesampling && go build ./...
```

Expected: no errors.

- [ ] **Step 4: Run unit tests**

```
cd processor/retroactivesampling && go test ./... -v
```

Expected: all PASS (integration tests are skipped without `-tags integration`).

- [ ] **Step 5: Run integration tests**

```
cd processor/retroactivesampling && go test -tags integration -v -timeout 30s
```

Expected: `TestE2E_ErrorPropagatesAcrossTwoProcessors` PASS.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/processor.go processor/retroactivesampling/integration_test.go
git commit -m "feat(buffer): remove onEvict, wire ring buffer into processor"
```

---

## Self-Review Checklist

### Spec coverage

| Spec requirement | Task |
|---|---|
| Pre-allocate ring file to exactly `maxBytes` | Task 1 (New) |
| Record format: traceID(32)+insertedAt(8)+dataLen(4)+data | Task 2 (WriteWithEviction) |
| Skip record on wrap | Task 4 |
| sweepOneLocked removes only swept delta, trace stays alive if more remain | Task 3 |
| sweepOneLocked handles `rHead + hdrSize > maxBytes` | Task 2 (sweepOneLocked) |
| Read: O(k) preads, merge in memory | Task 2 |
| Delete: remove from index, orphans swept naturally | Task 5 |
| Checkpoint saved on Close | Task 6 |
| Checkpoint loaded on New for instant restart | Task 6 |
| maxBytes mismatch → fresh start | Task 6 |
| Crash (no checkpoint) → fresh start | Task 6 |
| Remove onEvict from buffer.New | Task 7 |
| Remove onEvict from processor.go | Task 7 |
| integration_test.go MaxBufferBytes fix | Task 7 |
| `maxBytes <= 0` returns error | Task 1 |

All spec requirements covered. No gaps found.
