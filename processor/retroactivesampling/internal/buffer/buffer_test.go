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

func newTestBuffer(t *testing.T) *buffer.SpanBuffer {
	t.Helper()
	buf, err := buffer.New(t.TempDir(), 0, nil)
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

	err := buf.WriteWithEviction("trace1", traces, time.Now())
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

	err := buf.WriteWithEviction("trace2", t1, time.Now())
	require.NoError(t, err)
	err = buf.WriteWithEviction("trace2", t2, time.Now())
	require.NoError(t, err)

	got, ok, err := buf.Read("trace2")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.SpanCount())
}

func TestDelete(t *testing.T) {
	buf := newTestBuffer(t)
	traces := singleSpanTraces("trace3_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 100)

	err := buf.WriteWithEviction("trace3", traces, time.Now())
	require.NoError(t, err)
	require.NoError(t, buf.Delete("trace3"))

	_, ok, err := buf.Read("trace3")
	require.NoError(t, err)
	assert.False(t, ok)
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

func TestWriteWithEvictionBytesLimit(t *testing.T) {
	// Determine exact on-disk size of one trace file (header + proto).
	sample := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(sample)
	require.NoError(t, err)
	traceFileSize := int64(8 + len(data)) // 8-byte insertedAt header + proto

	buf, err := buffer.New(t.TempDir(), traceFileSize, nil) // capacity for exactly one trace
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	now := time.Now()
	trA := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	require.NoError(t, buf.WriteWithEviction("traceA", trA, now))

	trB := singleSpanTraces("traceB_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	require.NoError(t, buf.WriteWithEviction("traceB", trB, now.Add(time.Millisecond)))

	_, ok, _ := buf.Read("traceA")
	assert.False(t, ok, "oldest trace should have been evicted")
	_, ok, _ = buf.Read("traceB")
	assert.True(t, ok, "newest trace should be present")
}

func TestWriteWithEvictionFiresCallback(t *testing.T) {
	sample := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(sample)
	require.NoError(t, err)
	traceFileSize := int64(8 + len(data))

	var evictedID string
	var evictedAt time.Time
	buf, err := buffer.New(t.TempDir(), traceFileSize, func(id string, insertedAt time.Time) {
		evictedID = id
		evictedAt = insertedAt
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })

	insertTime := time.Now()
	trA := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	require.NoError(t, buf.WriteWithEviction("traceA", trA, insertTime))

	trB := singleSpanTraces("traceB_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	require.NoError(t, buf.WriteWithEviction("traceB", trB, insertTime.Add(time.Millisecond)))

	assert.Equal(t, "traceA", evictedID, "callback should receive evicted trace ID")
	assert.WithinDuration(t, insertTime, evictedAt, time.Second, "callback should receive original insertedAt")
}
