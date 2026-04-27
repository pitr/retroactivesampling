package buffer_test

import (
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

func newBufB(b *testing.B, maxBytes int64) *buffer.SpanBuffer {
	b.Helper()
	buf, err := buffer.New(filepath.Join(b.TempDir(), "buf.ring"), maxBytes, time.Hour, nil, nil)
	require.NoError(b, err)
	b.Cleanup(func() { _ = buf.Close() })
	return buf
}

// BenchmarkWrite measures write throughput under eviction pressure. The buffer
// is pre-filled to capacity before timing so every write in the timed loop
// triggers a sweep.
func BenchmarkWrite(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(b, err)
	rs := int64(28 + len(data)) // 28 = hdrSize

	bufSize := 2 * int64(4096)
	buf := newBufB(b, bufSize)
	now := time.Now().Add(-time.Second)

	for n := bufSize / rs; n > 0; n-- {
		require.NoError(b, buf.Write(traceA, data, now))
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = buf.Write(traceA, data, now)
	}
}

// BenchmarkRead measures Write + onMatch delivery throughput for interesting traces.
// Interest is pre-registered so the sweeper delivers each record immediately on encounter.
func BenchmarkRead(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(b, err)
	rs := int64(28 + len(data)) // 28 = hdrSize

	traceIDs := make([]pcommon.TraceID, b.N)
	for i := range traceIDs {
		binary.BigEndian.PutUint64(traceIDs[i][8:], uint64(i+1)) // +1 avoids the zero traceID (skip-record sentinel)
	}

	delivered := make(chan struct{}, b.N)
	onMatch := func(_ pcommon.TraceID, _ []byte) { delivered <- struct{}{} }

	buf, err := buffer.New(
		filepath.Join(b.TempDir(), "buf.ring"),
		int64(b.N)*rs+rs,
		time.Hour,
		onMatch,
		nil,
	)
	require.NoError(b, err)
	b.Cleanup(func() { _ = buf.Close() })

	// Register interest before writing so the sweeper delivers each record immediately.
	for _, tid := range traceIDs {
		buf.AddInterest(tid)
	}

	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for _, tid := range traceIDs {
		require.NoError(b, buf.Write(tid, data, now))
	}
	for range traceIDs {
		<-delivered
	}
}
