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
	buf, err := buffer.New(filepath.Join(b.TempDir(), "buf.ring"), maxBytes, time.Hour, buffer.DefaultStageCap, nil, nil)
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
		buffer.DefaultStageCap,
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

// BenchmarkWriteConcurrent measures write throughput under contention.
// 8 goroutines write to a ring sized so eviction is required.
func BenchmarkWriteConcurrent(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(b, err)

	bufSize := int64(64 * 1024) // 64 KiB ring
	buf := newBufB(b, bufSize)
	now := time.Now().Add(-time.Second)

	// Warm the ring near capacity.
	rs := int64(28 + len(data))
	for n := bufSize/rs - 4; n > 0; n-- {
		require.NoError(b, buf.Write(traceA, data, now))
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = buf.Write(traceA, data, now)
		}
	})
}

// BenchmarkWriteWithDelivery measures write throughput with 5% interest rate.
// Interesting records require sweeper handoff during eviction.
func BenchmarkWriteWithDelivery(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(b, err)
	rs := int64(28 + len(data))

	bufSize := int64(64 * 1024)
	buf, err := buffer.New(
		filepath.Join(b.TempDir(), "buf.ring"),
		bufSize,
		time.Hour,
		buffer.DefaultStageCap,
		func(_ pcommon.TraceID, _ []byte) {},
		nil,
	)
	require.NoError(b, err)
	b.Cleanup(func() { _ = buf.Close() })

	now := time.Now().Add(-time.Second)

	// Pre-fill near capacity.
	for n := bufSize/rs - 4; n > 0; n-- {
		require.NoError(b, buf.Write(traceA, data, now))
	}

	// Mark every 20th trace ID interesting (5%).
	for i := range 64 {
		var tid pcommon.TraceID
		binary.BigEndian.PutUint64(tid[8:], uint64(i*20+1))
		buf.AddInterest(tid)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		var tid pcommon.TraceID
		binary.BigEndian.PutUint64(tid[8:], uint64(i+1))
		_ = buf.Write(tid, data, now)
	}
}
