package buffer_test

import (
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

var randSource = rand.NewChaCha8([32]byte{})

func traceID() pcommon.TraceID {
	var id pcommon.TraceID
	randSource.Read(id[:])
	return id
}

func singleSpanTraces(traceID pcommon.TraceID, statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

func newBuf(t *testing.T, chunkSize int, chunks int, wait time.Duration, onMatch func(pcommon.TraceID, []byte)) *buffer.SpanBuffer {
	t.Helper()
	buf, err := buffer.New(
		filepath.Join(t.TempDir(), "buf.ring"),
		int64(chunkSize*chunks),
		chunkSize,
		wait,
		onMatch,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func TestNew_validation(t *testing.T) {
	dir := t.TempDir()
	const cs = 128 // 128-byte chunks; hdrSize=28, so at least 2 records fit (56 <= 128)

	t.Run("ok", func(t *testing.T) {
		buf, err := buffer.New(filepath.Join(dir, "ok.ring"), int64(cs*2), cs, time.Second, nil, nil)
		require.NoError(t, err)
		require.NoError(t, buf.Close())
	})
	t.Run("decisionWait zero", func(t *testing.T) {
		_, err := buffer.New(filepath.Join(dir, "a.ring"), int64(cs*4), cs, 0, nil, nil)
		require.Error(t, err)
	})
	t.Run("maxBytes rounds down below 2 chunks", func(t *testing.T) {
		// maxBytes = cs*2 - 1 rounds down to cs, which is < 2*cs
		_, err := buffer.New(filepath.Join(dir, "b.ring"), int64(cs*2-1), cs, time.Second, nil, nil)
		require.Error(t, err)
	})
	t.Run("chunkSize too small for 2 records", func(t *testing.T) {
		// hdrSize=28; chunkSize must be >= 56; use 55
		_, err := buffer.New(filepath.Join(dir, "c.ring"), int64(55*4), 55, time.Second, nil, nil)
		require.Error(t, err)
	})
}

var _ = sync.Mutex{} // ensure sync imported for later tests

// ---- Write path ----

func TestWrite_recordTooLarge(t *testing.T) {
	const cs = 128
	buf := newBuf(t, cs, 4, time.Hour, nil)
	data := make([]byte, cs) // hdrSize+cs > cs
	err := buf.Write(traceID(), data, time.Now())
	require.ErrorContains(t, err, "exceeds chunk size")
}

func TestWrite_fillsChunk(t *testing.T) {
	// chunkSize=128, data=32 → recSize=60; 2 fit (120≤128), 3rd triggers flush
	const cs = 128
	buf := newBuf(t, cs, 4, time.Hour, nil)
	data := make([]byte, 32)
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
	// Third write forces a flush; ring has 4 chunks so no ErrFull.
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
}

func TestWrite_full(t *testing.T) {
	// 2-chunk ring. chunkSize=128, data=32 → recSize=60.
	// rec1: stageN=60; rec2: stageN=120
	// rec3: 120+60>128 → flush chunk0 (used=128), stageN=60
	// rec4: stageN=120
	// rec5: 120+60>128 → flush chunk1 (used=256=maxBytes) → ErrFull (no staging)
	const cs = 128
	buf := newBuf(t, cs, 2, time.Hour, nil)
	data := make([]byte, 32)
	for i := 0; i < 4; i++ {
		require.NoError(t, buf.Write(traceID(), data, time.Now()))
	}
	err := buf.Write(traceID(), data, time.Now())
	require.ErrorIs(t, err, buffer.ErrFull)
}

// ---- Interest set ----

func TestInterest_basicAddHas(t *testing.T) {
	buf := newBuf(t, 128, 4, time.Hour, nil)
	id := traceID()
	require.False(t, buf.HasInterest(id))
	buf.AddInterest(id)
	require.True(t, buf.HasInterest(id))
}

func TestInterest_expiry(t *testing.T) {
	buf := newBuf(t, 128, 4, 50*time.Millisecond, nil)
	id := traceID()
	buf.AddInterest(id)
	require.True(t, buf.HasInterest(id))
	time.Sleep(60 * time.Millisecond)
	require.False(t, buf.HasInterest(id))
}

func TestInterest_renewOnAdd(t *testing.T) {
	buf := newBuf(t, 128, 4, 80*time.Millisecond, nil)
	id := traceID()
	buf.AddInterest(id)
	time.Sleep(50 * time.Millisecond)
	buf.AddInterest(id) // renew
	time.Sleep(50 * time.Millisecond)
	// 100ms total; renewed at 50ms → only 50ms since last add < 80ms → still present
	require.True(t, buf.HasInterest(id))
}

// ---- Sweeper delivery and eviction ----

func TestSweeper_delivery(t *testing.T) {
	const cs = 128
	var matched []pcommon.TraceID
	var mu sync.Mutex
	onMatch := func(id pcommon.TraceID, _ []byte) {
		mu.Lock()
		matched = append(matched, id)
		mu.Unlock()
	}

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(cs*4), cs, time.Hour, onMatch, nil)
	require.NoError(t, err)

	id := traceID()
	require.NoError(t, buf.Write(id, make([]byte, 32), time.Now()))
	require.NoError(t, buf.Flush())
	buf.AddInterest(id)
	require.NoError(t, buf.Close())

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, matched, id)
}

func TestSweeper_eviction(t *testing.T) {
	const cs = 128
	var evictions int
	var matched int
	evictObs := func(time.Duration) { evictions++ }
	onMatch := func(pcommon.TraceID, []byte) { matched++ }

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(cs*4), cs, 50*time.Millisecond, onMatch, evictObs)
	require.NoError(t, err)

	old := time.Now().Add(-time.Second) // well past decisionWait
	require.NoError(t, buf.Write(traceID(), make([]byte, 32), old))
	require.NoError(t, buf.Flush())
	require.NoError(t, buf.Close())

	require.Equal(t, 1, evictions)
	require.Equal(t, 0, matched)
}

func TestSweeper_pressure_eviction(t *testing.T) {
	// 4-chunk ring; decisionWait=1h so no age-based eviction.
	// Write 8 records (recSize=60, cs=128 → 2 per chunk → 4 chunks full via Close flush).
	// Close sets closed=true; sweeper pressure=true for all records → all evicted.
	const cs = 128
	var evictions int
	evictObs := func(time.Duration) { evictions++ }

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(cs*4), cs, time.Hour, nil, evictObs)
	require.NoError(t, err)

	data := make([]byte, 32) // recSize=60
	now := time.Now()
	// rec1: stageN=60; rec2: stageN=120
	// rec3: flush chunk0 (used=128), stageN=60; rec4: stageN=120
	// rec5: flush chunk1 (used=256), stageN=60; rec6: stageN=120
	// rec7: flush chunk2 (used=384), stageN=60; rec8: stageN=120
	for i := 0; i < 8; i++ {
		require.NoError(t, buf.Write(traceID(), data, now))
	}
	// Close flushes stage (chunk3, used=512) then sweeper drains with closed=true.
	require.NoError(t, buf.Close())

	require.Equal(t, 8, evictions)
}

func TestClose_flushesStage(t *testing.T) {
	const cs = 128
	var matched []pcommon.TraceID
	var mu sync.Mutex
	onMatch := func(id pcommon.TraceID, _ []byte) {
		mu.Lock()
		matched = append(matched, id)
		mu.Unlock()
	}

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(cs*4), cs, time.Hour, onMatch, nil)
	require.NoError(t, err)

	id := traceID()
	// 1 record in stage (stageN=60); no explicit Flush — Close must flush automatically.
	require.NoError(t, buf.Write(id, make([]byte, 32), time.Now()))
	buf.AddInterest(id)
	require.NoError(t, buf.Close())

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, matched, id)
}
