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
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), maxBytes)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func TestNewCreatesFileAtPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.ring")
	buf, err := buffer.New(path, 1<<20)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, info.IsDir(), "buffer path must be a regular file, not a directory")
}

func TestNewRejectsZeroMaxBytes(t *testing.T) {
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 0)
	assert.Error(t, err)
}

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

func TestEviction(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
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
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	buf := newBufSize(t, 2*rs+1)

	now := time.Now()
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100), now))
	require.NoError(t, buf.WriteWithEviction(traceA, singleSpanTraces(traceA, ptrace.StatusCodeOk, 200), now.Add(time.Millisecond)))
	require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), now.Add(2*time.Millisecond)))

	got, ok, err := buf.Read(traceA)
	require.NoError(t, err)
	require.True(t, ok, "traceA should still be readable via its second delta")
	assert.Equal(t, 1, got.SpanCount())

	got, ok, err = buf.Read(traceB)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestWrap(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// 5 bytes left after 2 records — too small for a skip record header (< 44), no skip written.
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
	require.NoError(t, buf.Delete(traceA))

	// Ring used=rs. Writing traceB triggers sweep of orphaned traceA record.
	// sweepOneLocked sees traceA in ring but not in entries — just advances rHead.
	require.NoError(t, buf.WriteWithEviction(traceB, singleSpanTraces(traceB, ptrace.StatusCodeOk, 100), now.Add(time.Millisecond)))

	_, ok, _ := buf.Read(traceA)
	assert.False(t, ok)
	_, ok, _ = buf.Read(traceB)
	assert.True(t, ok)
}

func TestWrapWithSkipRecord(t *testing.T) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	rs := recSize(t, tr)
	// 50 bytes left after 2 records — >= 44, so wrapLocked writes a skip record.
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
