package buffer_test

import (
	"fmt"
	"os"
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
// is sized to 2 memory pages, so writes trigger eviction once full,
// exercising the steady-state hot path.
func BenchmarkWrite(b *testing.B) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)

	buf := newBufSizeB(b, 2*int64(os.Getpagesize()))
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buf.WriteWithEviction(traceA, tr, now)
	}
}

// BenchmarkRead measures ReadAndDelete throughput. The buffer is pre-populated
// with b.N entries before the timed loop starts so no writes occur during timing.
func BenchmarkRead(b *testing.B) {
	tr := singleSpanTraces(traceA, ptrace.StatusCodeOk, 100)

	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(tr)
	require.NoError(b, err)
	rs := int64(44 + len(data))

	traceIDs := make([]string, b.N)
	for i := range traceIDs {
		traceIDs[i] = fmt.Sprintf("%032d", i)
	}

	buf := newBufSizeB(b, int64(b.N)*rs+rs)
	now := time.Now()
	for _, tid := range traceIDs {
		require.NoError(b, buf.WriteWithEviction(tid, tr, now))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for _, tid := range traceIDs {
		_, _, _ = buf.ReadAndDelete(tid)
	}
}
