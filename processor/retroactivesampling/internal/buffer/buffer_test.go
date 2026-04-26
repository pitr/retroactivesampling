package buffer_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

var (
	traceA = [16]byte{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa}
	traceB = [16]byte{0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb}
	traceC = [16]byte{0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc}
)

func singleSpanTraces(traceID [16]byte, statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	span.SetTraceID(pcommon.TraceID(traceID))
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

// recSize returns the exact ring record size (header + proto) for a Traces value.
func recSize(t *testing.T, spans ptrace.Traces) int64 {
	t.Helper()
	return int64(28 + len(marshalT(t, spans))) // 28 = hdrSize: traceID(16) + insertedAt(8) + dataLen(4)
}

func marshalT(t *testing.T, tr ptrace.Traces) []byte {
	t.Helper()
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(tr)
	require.NoError(t, err)
	return data
}

func readSpans(t *testing.T, buf *buffer.SpanBuffer, id [16]byte) (ptrace.Traces, bool) {
	t.Helper()
	chunks, err := buf.ReadAndDelete(id)
	require.NoError(t, err)
	if len(chunks) == 0 {
		return ptrace.Traces{}, false
	}
	result := ptrace.NewTraces()
	u := ptrace.ProtoUnmarshaler{}
	for _, chunk := range chunks {
		tr, err := u.UnmarshalTraces(chunk)
		require.NoError(t, err)
		tr.ResourceSpans().MoveAndAppendTo(result.ResourceSpans())
	}
	return result, true
}

func newBuf(t *testing.T) *buffer.SpanBuffer {
	return newBufSize(t, 1<<20)
}

func newBufSize(t *testing.T, maxBytes int64) *buffer.SpanBuffer {
	return newBufSizeWithObserver(t, maxBytes, nil)
}

func newBufSizeWithObserver(t *testing.T, maxBytes int64, obs func(time.Duration)) *buffer.SpanBuffer {
	t.Helper()
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), maxBytes, obs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func TestNewCreatesFileAtPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.ring")
	buf, err := buffer.New(path, 1<<20, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, info.IsDir(), "buffer path must be a regular file, not a directory")
}

func TestNewRejectsZeroMaxBytes(t *testing.T) {
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 0, nil)
	assert.Error(t, err)
}

func TestEviction(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	buf := newBufSize(t, rs)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)), now))
	require.NoError(t, buf.WriteWithEviction(traceB, marshalT(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100)), now.Add(time.Millisecond)))

	chunks, _ := buf.ReadAndDelete(traceA)
	assert.Empty(t, chunks, "traceA should be evicted")
	chunks, _ = buf.ReadAndDelete(traceB)
	assert.NotEmpty(t, chunks, "traceB should be present")
}

func TestPartialDeltaEviction(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	buf := newBufSize(t, 2*rs+1)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)), now))
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 200)), now.Add(time.Millisecond)))
	require.NoError(t, buf.WriteWithEviction(traceB, marshalT(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100)), now.Add(2*time.Millisecond)))

	rsA200 := recSize(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 200))
	rsB := recSize(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100))
	assert.Equal(t, rsA200+rsB, buf.LiveBytes(), "first traceA delta evicted, second + traceB are live")
	assert.Greater(t, buf.OrphanedBytes(), int64(0), "evicted first delta remains in ring until swept")

	got, ok := readSpans(t, buf, traceA)
	require.True(t, ok, "traceA should still be readable via its second delta")
	assert.Equal(t, 1, got.SpanCount())

	got, ok = readSpans(t, buf, traceB)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestWrap(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// 5 bytes left after 2 records — too small for a skip record header (< 28), no skip written.
	buf := newBufSize(t, 2*rs+5)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)), now))
	require.NoError(t, buf.WriteWithEviction(traceB, marshalT(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100)), now.Add(time.Millisecond)))
	require.NoError(t, buf.WriteWithEviction(traceC, marshalT(t, singleSpanTraces(traceC, ptrace.StatusCodeOk, 100)), now.Add(2*time.Millisecond)))

	chunks, _ := buf.ReadAndDelete(traceA)
	assert.Empty(t, chunks, "traceA should be evicted after wrap")

	got, ok := readSpans(t, buf, traceB)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())

	got, ok = readSpans(t, buf, traceC)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestEvictionObserverCalledForLiveRecord(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)

	var observed []time.Duration
	buf := newBufSizeWithObserver(t, rs, func(d time.Duration) {
		observed = append(observed, d)
	})

	past := time.Now().Add(-50 * time.Millisecond)
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)), past))
	require.NoError(t, buf.WriteWithEviction(traceB, marshalT(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100)), time.Now()))

	require.Len(t, observed, 1, "observer called once for the one live eviction")
	assert.Greater(t, observed[0], time.Duration(0), "observed duration must be positive")
}

func TestWrapWithSkipRecord(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// 50 bytes left after 2 records — >= 28, so wrapLocked writes a skip record.
	buf := newBufSize(t, 2*rs+50)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)), now))
	require.NoError(t, buf.WriteWithEviction(traceB, marshalT(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100)), now.Add(time.Millisecond)))
	require.NoError(t, buf.WriteWithEviction(traceC, marshalT(t, singleSpanTraces(traceC, ptrace.StatusCodeOk, 100)), now.Add(2*time.Millisecond)))

	// The skip/pad record at the tail is orphaned (never indexed).
	assert.Greater(t, buf.OrphanedBytes(), int64(0), "pad record bytes are orphaned immediately")

	chunks, _ := buf.ReadAndDelete(traceA)
	assert.Empty(t, chunks, "traceA should be evicted after wrap with skip record")

	got, ok := readSpans(t, buf, traceC)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestReadAndDelete(t *testing.T) {
	buf := newBuf(t)
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, tr), time.Now()))

	got, ok := readSpans(t, buf, traceA)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())

	// Second call: entry gone.
	chunks, _ := buf.ReadAndDelete(traceA)
	assert.Empty(t, chunks)
}

func TestReadAndDeleteMissing(t *testing.T) {
	buf := newBuf(t)
	chunks, _ := buf.ReadAndDelete(traceA)
	assert.Empty(t, chunks)
}

func TestReadAndDeleteMultipleDeltas(t *testing.T) {
	buf := newBuf(t)
	t1 := singleSpanTraces(traceA, ptrace.StatusCodeOk, 50)
	t2 := singleSpanTraces(traceA, ptrace.StatusCodeOk, 60)
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, t1), time.Now()))
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, t2), time.Now()))

	got, ok := readSpans(t, buf, traceA)
	require.True(t, ok)
	assert.Equal(t, 2, got.SpanCount())
	assert.Equal(t, int64(0), buf.LiveBytes(), "both deltas removed from live index")
	assert.Greater(t, buf.OrphanedBytes(), int64(0), "bytes remain in ring until swept")

	chunks, _ := buf.ReadAndDelete(traceA)
	assert.Empty(t, chunks)
}

func TestLiveBytesAndOrphanedBytes(t *testing.T) {
	buf := newBuf(t)
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)

	assert.Equal(t, int64(0), buf.LiveBytes())
	assert.Equal(t, int64(0), buf.OrphanedBytes())

	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, tr), time.Now()))
	assert.Equal(t, rs, buf.LiveBytes())
	assert.Equal(t, int64(0), buf.OrphanedBytes())

	chunks, _ := buf.ReadAndDelete(traceA)
	require.NotEmpty(t, chunks)
	assert.Equal(t, int64(0), buf.LiveBytes())
	assert.Equal(t, rs, buf.OrphanedBytes())
}

func TestLiveBytesDecrementedOnEviction(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	buf := newBufSize(t, rs)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)), now))
	require.NoError(t, buf.WriteWithEviction(traceB, marshalT(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100)), now.Add(time.Millisecond)))

	// traceA was evicted by sweepOneLocked; liveBytes must drop, orphaned must be zero.
	assert.Equal(t, rs, buf.LiveBytes(), "only traceB remains live")
	assert.Equal(t, int64(0), buf.OrphanedBytes())
}

func TestOrphanedBytesDecreasedAfterSweep(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	buf := newBufSize(t, rs)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)), now))

	chunks, _ := buf.ReadAndDelete(traceA)
	require.NotEmpty(t, chunks)

	orphanedBefore := buf.OrphanedBytes()
	assert.Equal(t, rs, orphanedBefore, "orphaned bytes should equal the evicted record")

	// Writing traceB triggers sweepOneLocked which sweeps the orphaned traceA record.
	require.NoError(t, buf.WriteWithEviction(traceB, marshalT(t, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100)), now.Add(time.Millisecond)))

	assert.Less(t, buf.OrphanedBytes(), orphanedBefore, "orphaned bytes should decrease after sweep")
}
