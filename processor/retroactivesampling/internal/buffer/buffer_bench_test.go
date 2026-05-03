package buffer_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

// BenchmarkWrite measures write throughput under eviction pressure. The buffer
// is pre-filled to capacity before timing so every write in the timed loop
// triggers a sweep.
func BenchmarkWrite(b *testing.B) {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(singleSpanTraces(traceID(), ptrace.StatusCodeOk, 100))
	require.NoError(b, err)

	ps := os.Getpagesize()
	buf, err := buffer.New(filepath.Join(b.TempDir(), "buf.ring"), int64(ps*2), time.Hour, nil, nil)
	require.NoError(b, err)

	now := time.Now().Add(-time.Second)
	for range int64(ps * 2) {
		err := buf.Write(traceID(), data, now)
		if err == buffer.ErrFull {
			break
		}
		require.NoError(b, err)
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = buf.Write(traceID(), data, now)
	}
}
