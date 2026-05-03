package buffer_test

import (
	"math/rand/v2"
	"os"
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
	_, _ = randSource.Read(id[:])
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

var _ = sync.Mutex{} // ensure sync imported for later tests

// ---- Write path ----

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

func TestWrite_fillsChunk(t *testing.T) {
	// recSize = ps/2; 2 fit per chunk (stageN reaches ps on write 2), 3rd triggers flush.
	ps := os.Getpagesize()
	buf := newBuf(t, 4, time.Hour, nil)
	data := make([]byte, ps/2-28) // recSize = ps/2
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
	// Third write: stageN+recSize > ps → flush chunk0, write 3rd into fresh stage.
	require.NoError(t, buf.Write(traceID(), data, time.Now()))
}

func TestWrite_full(t *testing.T) {
	// 2-chunk ring. recSize = ps/2; 2 per chunk.
	// Writes 1-2: stageN=ps. Write 3: flush chunk0 (used=ps), stageN=ps/2.
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

// ---- Interest set ----

func TestInterest_basicAddHas(t *testing.T) {
	buf := newBuf(t, 4, time.Hour, nil)
	id := traceID()
	require.False(t, buf.HasInterest(id))
	buf.AddInterest(id)
	require.True(t, buf.HasInterest(id))
}

func TestInterest_expiry(t *testing.T) {
	const dw = 50 * time.Millisecond
	buf := newBuf(t, 4, dw, nil)
	id := traceID()
	buf.AddInterest(id)
	require.True(t, buf.HasInterest(id))
	time.Sleep(dw + 200*time.Millisecond) // 4x margin against scheduler jitter
	require.False(t, buf.HasInterest(id))
}

func TestInterest_renewOnAdd(t *testing.T) {
	const dw = 200 * time.Millisecond
	buf := newBuf(t, 4, dw, nil)
	id := traceID()
	buf.AddInterest(id)
	time.Sleep(100 * time.Millisecond) // half the TTL
	buf.AddInterest(id)                // renew; clock resets
	time.Sleep(100 * time.Millisecond) // only 100ms since renewal, 100ms margin remaining
	require.True(t, buf.HasInterest(id))
}

// ---- Sweeper delivery and eviction ----

func TestSweeper_delivery(t *testing.T) {
	var matched []pcommon.TraceID
	var mu sync.Mutex
	onMatch := func(id pcommon.TraceID, _ []byte) {
		mu.Lock()
		matched = append(matched, id)
		mu.Unlock()
	}

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(os.Getpagesize()*4), time.Hour, onMatch, nil)
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
	var evictions int
	var matched int
	evictObs := func(time.Duration) { evictions++ }
	onMatch := func(pcommon.TraceID, []byte) { matched++ }

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(os.Getpagesize()*4), 50*time.Millisecond, onMatch, evictObs)
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
	// recSize = ps/2; 2 per chunk. 8 records fill 4 chunks.
	// Close sets closed=true; sweeper pressure=true for all records → all evicted.
	ps := os.Getpagesize()
	var evictions int
	evictObs := func(time.Duration) { evictions++ }

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(ps*4), time.Hour, nil, evictObs)
	require.NoError(t, err)

	data := make([]byte, ps/2-28) // recSize = ps/2; 2 per chunk
	now := time.Now()
	for range 8 {
		require.NoError(t, buf.Write(traceID(), data, now))
	}
	require.NoError(t, buf.Close())

	require.Equal(t, 8, evictions)
}

func TestClose_flushesStage(t *testing.T) {
	var matched []pcommon.TraceID
	var mu sync.Mutex
	onMatch := func(id pcommon.TraceID, _ []byte) {
		mu.Lock()
		matched = append(matched, id)
		mu.Unlock()
	}

	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), int64(os.Getpagesize()*4), time.Hour, onMatch, nil)
	require.NoError(t, err)

	id := traceID()
	// 1 record in stage; no explicit Flush — Close must flush automatically.
	require.NoError(t, buf.Write(id, make([]byte, 32), time.Now()))
	buf.AddInterest(id)
	require.NoError(t, buf.Close())

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, matched, id)
}
