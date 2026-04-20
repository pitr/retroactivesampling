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
