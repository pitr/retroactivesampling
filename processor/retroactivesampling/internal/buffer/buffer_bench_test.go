package buffer_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

func newBufSizeB(b *testing.B, maxBytes int64) *buffer.SpanBuffer {
	b.Helper()
	buf, err := buffer.New(filepath.Join(b.TempDir(), "buf.ring"), maxBytes, nil)
	require.NoError(b, err)
	b.Cleanup(func() { _ = buf.Close() })
	return buf
}

// BenchmarkWrite measures write throughput under eviction pressure. The buffer
// holds exactly two records, so every write after warm-up triggers an eviction,
// exercising the steady-state hot path.
func BenchmarkWrite(b *testing.B) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(tr)
	require.NoError(b, err)
	rs := int64(44 + len(data))

	buf := newBufSizeB(b, 2*rs)
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buf.WriteWithEviction(traceA, tr, now)
	}
}

// BenchmarkRead measures read throughput. The write is excluded from timing
// via StopTimer/StartTimer so only ReadAndDelete is measured.
func BenchmarkRead(b *testing.B) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)
	buf := newBufSizeB(b, 1<<20) // 1 MB
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		_ = buf.WriteWithEviction(traceA, tr, now)
		b.StartTimer()
		_, _, _ = buf.ReadAndDelete(traceA)
	}
}
