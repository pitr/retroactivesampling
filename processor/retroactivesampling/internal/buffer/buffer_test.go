package buffer_test

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
)

var (
	traceA = pcommon.TraceID([16]byte{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa})
	traceB = pcommon.TraceID([16]byte{0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb})
	traceC = pcommon.TraceID([16]byte{0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc})
)

func marshalT(t *testing.T, tr ptrace.Traces) []byte {
	t.Helper()
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(tr)
	require.NoError(t, err)
	return data
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

func recSize(t *testing.T, spans ptrace.Traces) int64 {
	t.Helper()
	return int64(28 + len(marshalT(t, spans)))
}

// collector gathers onMatch deliveries keyed by trace ID.
type collector struct {
	mu     sync.Mutex
	chunks map[pcommon.TraceID][][]byte
}

func newCollector() *collector {
	return &collector{chunks: make(map[pcommon.TraceID][][]byte)}
}

func (c *collector) onMatch(tid pcommon.TraceID, data []byte) {
	c.mu.Lock()
	cp := make([]byte, len(data))
	copy(cp, data)
	c.chunks[tid] = append(c.chunks[tid], cp)
	c.mu.Unlock()
}

func (c *collector) get(tid pcommon.TraceID) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chunks[tid]
}

func (c *collector) waitFor(t *testing.T, tid pcommon.TraceID, wantChunks int, timeout time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := c.get(tid); len(got) >= wantChunks {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d chunk(s) for trace %v", wantChunks, tid)
	return nil
}

func newBuf(t *testing.T, maxBytes int64, decisionWait time.Duration, col *collector, evictObs func(time.Duration)) *buffer.SpanBuffer {
	t.Helper()
	var onMatch func(pcommon.TraceID, []byte)
	if col != nil {
		onMatch = col.onMatch
	}
	stageCap := buffer.DefaultStageCap
	if maxBytes < int64(stageCap)*2 {
		stageCap = int(maxBytes / 2)
		if stageCap < 28*2 {
			stageCap = 28 * 2
		}
	}
	buf, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), maxBytes, decisionWait, stageCap, onMatch, evictObs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func TestNewCreatesFileAtPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.ring")
	buf, err := buffer.New(path, 1<<20, time.Second, buffer.DefaultStageCap, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
}

func TestNewRejectsZeroMaxBytes(t *testing.T) {
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 0, time.Second, buffer.DefaultStageCap, nil, nil)
	assert.Error(t, err)
}

func TestNewRejectsZeroDecisionWait(t *testing.T) {
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, 0, buffer.DefaultStageCap, nil, nil)
	assert.Error(t, err)
}

// TestInterestDelivery: write a record, add interest, sweeper delivers it.
func TestInterestDelivery(t *testing.T) {
	col := newCollector()
	buf := newBuf(t, 1<<20, 5*time.Second, col, nil)
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(t, buf.Write(traceA, data, time.Now()))
	buf.AddInterest(traceA)
	chunks := col.waitFor(t, traceA, 1, 2*time.Second)
	assert.Len(t, chunks, 1)
	assert.Equal(t, data, chunks[0])
}

// TestMultipleDeltas: two writes for same trace, both delivered.
func TestMultipleDeltas(t *testing.T) {
	col := newCollector()
	buf := newBuf(t, 1<<20, 5*time.Second, col, nil)
	d1 := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 50))
	d2 := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 60))
	require.NoError(t, buf.Write(traceA, d1, time.Now()))
	require.NoError(t, buf.Write(traceA, d2, time.Now()))
	buf.AddInterest(traceA)
	chunks := col.waitFor(t, traceA, 2, 2*time.Second)
	assert.Len(t, chunks, 2)
}

// TestEvictionObserver: non-interesting record is swept after decisionWait and
// evictObserver is called.
func TestEvictionObserver(t *testing.T) {
	observed := make(chan time.Duration, 1)
	buf := newBuf(t, 1<<20, 10*time.Millisecond, nil, func(d time.Duration) {
		select {
		case observed <- d:
		default:
		}
	})
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(t, buf.Write(traceA, data, time.Now().Add(-20*time.Millisecond)))
	select {
	case d := <-observed:
		assert.Greater(t, d, time.Duration(0))
	case <-time.After(2 * time.Second):
		t.Fatal("evictObserver not called")
	}
}

// TestDecisionWaitHonoured: record is not swept before decisionWait, then is.
func TestDecisionWaitHonoured(t *testing.T) {
	observed := make(chan struct{}, 1)
	buf := newBuf(t, 1<<20, 50*time.Millisecond, nil, func(time.Duration) {
		select {
		case observed <- struct{}{}:
		default:
		}
	})
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	require.NoError(t, buf.Write(traceA, data, time.Now()))
	select {
	case <-observed:
		t.Fatal("record swept before decisionWait expired")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("record not swept after decisionWait")
	}
}

// TestHasInterest: reflects the interest cache state.
func TestHasInterest(t *testing.T) {
	buf := newBuf(t, 1<<20, 5*time.Second, nil, nil)
	assert.False(t, buf.HasInterest(traceA))
	buf.AddInterest(traceA)
	assert.True(t, buf.HasInterest(traceA))
}

// TestWrap: ring wraps correctly; records after wrap are delivered.
// Interest must be registered before writing so the sweeper finds it in the
// cache when it reaches the record.
func TestWrap(t *testing.T) {
	col := newCollector()
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	rs := recSize(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	// two records + 5 bytes slack (forces a tail gap on third write, no skip record)
	buf := newBuf(t, 2*rs+5, 5*time.Second, col, nil)

	now := time.Now()
	require.NoError(t, buf.Write(traceA, data, now))
	require.NoError(t, buf.Write(traceB, data, now))
	buf.AddInterest(traceC)
	require.NoError(t, buf.Write(traceC, data, now))

	chunks := col.waitFor(t, traceC, 1, 2*time.Second)
	assert.Len(t, chunks, 1)
}

// TestWrapWithSkipRecord: skip record at tail is handled transparently.
func TestWrapWithSkipRecord(t *testing.T) {
	col := newCollector()
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	rs := recSize(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	buf := newBuf(t, 2*rs+50, 5*time.Second, col, nil)

	require.NoError(t, buf.Write(traceA, data, time.Now().Add(-time.Second)))
	require.NoError(t, buf.Write(traceB, data, time.Now().Add(-time.Second)))
	require.NoError(t, buf.Write(traceC, data, time.Now().Add(-time.Second)))
	buf.AddInterest(traceC)

	chunks := col.waitFor(t, traceC, 1, 2*time.Second)
	assert.Len(t, chunks, 1)
}

func TestNewRejectsTinyStageCap(t *testing.T) {
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, time.Second, buffer.DefaultStageCap, nil, nil)
	require.NoError(t, err)
	_, err = buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, time.Second, 8, nil, nil)
	assert.Error(t, err)
}

func TestNewRejectsStageCapTooLarge(t *testing.T) {
	// stageCap > maxBytes/2 is rejected
	_, err := buffer.New(filepath.Join(t.TempDir(), "buf.ring"), 1024, time.Second, 800, nil, nil)
	assert.Error(t, err)
}

// TestEvictionUnderPressure: when ring is full the writer unblocks after sweep.
func TestEvictionUnderPressure(t *testing.T) {
	col := newCollector()
	data := marshalT(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	rs := recSize(t, singleSpanTraces(traceA, ptrace.StatusCodeOk, 100))
	buf := newBuf(t, rs, 10*time.Second, col, nil)

	// First write fills ring.
	require.NoError(t, buf.Write(traceA, data, time.Now()))

	done := make(chan error, 1)
	go func() {
		// Second write must block until first is swept (ring full, sweeper ignores decisionWait).
		done <- buf.Write(traceB, data, time.Now())
	}()

	buf.AddInterest(traceB)
	select {
	case err := <-done:
		require.NoError(t, err)
		chunks := col.waitFor(t, traceB, 1, 2*time.Second)
		assert.Len(t, chunks, 1)
	case <-time.After(3 * time.Second):
		t.Fatal("Write blocked indefinitely under ring pressure")
	}
}
