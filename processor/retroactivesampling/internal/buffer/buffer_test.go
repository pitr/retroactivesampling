package buffer_test

import (
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

var randSource = rand.NewChaCha8([32]byte{})

func traceID() pcommon.TraceID {
	var id pcommon.TraceID
	randSource.Read(id[:])
	return id
}

func singleSpanTraces(traceID pcommon.TraceID, statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

func newBuf(t *testing.T, chunkSize int, chunks int, wait time.Duration, onMatch func(pcommon.TraceID, []byte)) *buffer.SpanBuffer {
	t.Helper()
	buf, err := buffer.New(
		filepath.Join(t.TempDir(), "buf.ring"),
		int64(chunkSize*chunks),
		chunkSize,
		wait,
		onMatch,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func TestNew_validation(t *testing.T) {
	dir := t.TempDir()
	const cs = 128 // 128-byte chunks; hdrSize=28, so at least 2 records fit (56 <= 128)

	t.Run("ok", func(t *testing.T) {
		buf, err := buffer.New(filepath.Join(dir, "ok.ring"), int64(cs*2), cs, time.Second, nil, nil)
		require.NoError(t, err)
		require.NoError(t, buf.Close())
	})
	t.Run("decisionWait zero", func(t *testing.T) {
		_, err := buffer.New(filepath.Join(dir, "a.ring"), int64(cs*4), cs, 0, nil, nil)
		require.Error(t, err)
	})
	t.Run("maxBytes rounds down below 2 chunks", func(t *testing.T) {
		// maxBytes = cs*2 - 1 rounds down to cs, which is < 2*cs
		_, err := buffer.New(filepath.Join(dir, "b.ring"), int64(cs*2-1), cs, time.Second, nil, nil)
		require.Error(t, err)
	})
	t.Run("chunkSize too small for 2 records", func(t *testing.T) {
		// hdrSize=28; chunkSize must be >= 56; use 55
		_, err := buffer.New(filepath.Join(dir, "c.ring"), int64(55*4), 55, time.Second, nil, nil)
		require.Error(t, err)
	})
}

var _ = sync.Mutex{} // ensure sync imported for later tests
