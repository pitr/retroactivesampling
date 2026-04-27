package buffer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// TestStagingAmortizesFlush verifies that writes accumulate in the stage and
// that a single Pwrite covers many records.
func TestStagingAmortizesFlush(t *testing.T) {
	traceA := pcommon.TraceID([16]byte{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa})

	buf, err := New(filepath.Join(t.TempDir(), "buf.ring"), 1<<20, time.Hour, 4096, nil, nil)
	require.NoError(t, err)
	defer func() { _ = buf.Close() }()

	payload := make([]byte, 100)
	recSize := hdrSize + len(payload) // 28 + 100 = 128
	maxRecordsInStage := 4096 / recSize

	for i := 0; i < maxRecordsInStage; i++ {
		require.NoError(t, buf.Write(traceA, payload, time.Now()))
	}

	buf.mu.Lock()
	stagedBytes := len(buf.stage)
	wHead := buf.wHead
	buf.mu.Unlock()

	require.Greater(t, stagedBytes, 0, "stage should hold records before overflow")
	require.Equal(t, int64(0), wHead, "no flush should have happened yet")

	// One more write triggers flush.
	require.NoError(t, buf.Write(traceA, payload, time.Now()))

	buf.mu.Lock()
	wHead = buf.wHead
	stagedBytes = len(buf.stage)
	buf.mu.Unlock()

	require.Greater(t, wHead, int64(0), "flush should have advanced wHead")
	require.Equal(t, recSize, stagedBytes, "only the most recent record is in stage")
}
