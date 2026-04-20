package buffer_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

func newTestBuffer(t *testing.T) *buffer.SpanBuffer {
	t.Helper()
	f, err := os.CreateTemp("", "buffer-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	buf, err := buffer.New(f.Name(), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func singleSpanTraces(traceID string, statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	var tid [16]byte
	copy(tid[:], traceID)
	span.SetTraceID(tid)
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

func TestWriteAndRead(t *testing.T) {
	buf := newTestBuffer(t)
	traces := singleSpanTraces("trace1_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 100)

	_, err := buf.Write("trace1", traces, time.Now())
	require.NoError(t, err)

	got, ok, err := buf.Read("trace1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestWriteAppendsSpans(t *testing.T) {
	buf := newTestBuffer(t)
	t1 := singleSpanTraces("trace2_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 50)
	t2 := singleSpanTraces("trace2_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 60)

	_, err := buf.Write("trace2", t1, time.Now())
	require.NoError(t, err)
	_, err = buf.Write("trace2", t2, time.Now())
	require.NoError(t, err)

	got, ok, err := buf.Read("trace2")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.SpanCount())
}

func TestDelete(t *testing.T) {
	buf := newTestBuffer(t)
	traces := singleSpanTraces("trace3_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 100)

	_, err := buf.Write("trace3", traces, time.Now())
	require.NoError(t, err)
	require.NoError(t, buf.Delete("trace3"))

	_, ok, err := buf.Read("trace3")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvictOldestFIFO(t *testing.T) {
	buf := newTestBuffer(t)
	now := time.Now()
	t1 := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	t2 := singleSpanTraces("traceB_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)

	_, err := buf.Write("traceA", t1, now)
	require.NoError(t, err)
	_, err = buf.Write("traceB", t2, now.Add(time.Millisecond))
	require.NoError(t, err)

	evicted, err := buf.EvictOldest()
	require.NoError(t, err)
	assert.Equal(t, "traceA", evicted, "oldest trace should be evicted first")

	_, ok, _ := buf.Read("traceA")
	assert.False(t, ok, "evicted trace should be gone")

	_, ok, _ = buf.Read("traceB")
	assert.True(t, ok, "newer trace should remain")
}

func TestEvictOldestOnEmptyBuffer(t *testing.T) {
	buf := newTestBuffer(t)
	evicted, err := buf.EvictOldest()
	require.NoError(t, err)
	assert.Equal(t, "", evicted, "empty buffer should return empty string")
}

func TestReadMissingTrace(t *testing.T) {
	buf := newTestBuffer(t)
	_, ok, err := buf.Read("nonexistent")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDeleteNonExistent(t *testing.T) {
	buf := newTestBuffer(t)
	err := buf.Delete("nonexistent")
	require.NoError(t, err, "delete of nonexistent trace should be idempotent")
}

func TestWriteWithEvictionCountLimit(t *testing.T) {
	f, err := os.CreateTemp("", "buffer-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	buf, err := buffer.New(f.Name(), 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	now := time.Now()
	for i, id := range []string{"traceA", "traceB"} {
		tr := singleSpanTraces(id+"_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
		require.NoError(t, buf.WriteWithEviction(id, tr, now.Add(time.Duration(i)*time.Millisecond)))
	}
	assert.Equal(t, int64(2), buf.Count())

	// Writing a third trace must evict the oldest (traceA).
	tr := singleSpanTraces("traceC_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	require.NoError(t, buf.WriteWithEviction("traceC", tr, now.Add(2*time.Millisecond)))
	assert.Equal(t, int64(2), buf.Count())

	_, ok, _ := buf.Read("traceA")
	assert.False(t, ok, "oldest trace should have been evicted")
	_, ok, _ = buf.Read("traceC")
	assert.True(t, ok, "newest trace should be present")
}
