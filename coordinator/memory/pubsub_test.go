package memory_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pitr.ca/retroactivesampling/coordinator/memory"
)

func TestPublishNovel(t *testing.T) {
	ps := memory.New(time.Minute, nil)
	novel, err := ps.Publish(t.Context(), []byte("trace1"))
	require.NoError(t, err)
	assert.True(t, novel)
}

func TestPublishDuplicate(t *testing.T) {
	ps := memory.New(time.Minute, nil)
	ctx := t.Context()
	_, _ = ps.Publish(ctx, []byte("trace1"))
	novel, err := ps.Publish(ctx, []byte("trace1"))
	require.NoError(t, err)
	assert.False(t, novel)
}

func TestPublishCallsHandler(t *testing.T) {
	received := make(chan []byte, 1)
	ps := memory.New(time.Minute, func(id []byte) { received <- id })

	_, err := ps.Publish(t.Context(), []byte("trace2"))
	require.NoError(t, err)

	select {
	case id := <-received:
		assert.Equal(t, []byte("trace2"), id)
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}
}

func TestPublishDuplicateDoesNotCallHandler(t *testing.T) {
	var called atomic.Int32
	ps := memory.New(time.Minute, func([]byte) { called.Add(1) })

	ctx := t.Context()
	_, _ = ps.Publish(ctx, []byte("trace3"))
	_, _ = ps.Publish(ctx, []byte("trace3"))

	assert.Equal(t, int32(1), called.Load())
}

func TestTTLExpiry(t *testing.T) {
	ps := memory.New(100*time.Millisecond, nil)
	ctx := t.Context()

	novel, _ := ps.Publish(ctx, []byte("traceX"))
	assert.True(t, novel)

	time.Sleep(200 * time.Millisecond)

	novel, _ = ps.Publish(ctx, []byte("traceX"))
	assert.True(t, novel, "should be novel again after TTL expiry")
}

func TestConcurrentPublish(t *testing.T) {
	ps := memory.New(time.Minute, nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]bool, 100)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			novel, _ := ps.Publish(ctx, []byte("sametrace"))
			results[i] = novel
		}(i)
	}
	wg.Wait()

	trueCount := 0
	for _, v := range results {
		if v {
			trueCount++
		}
	}
	assert.Equal(t, 1, trueCount, "exactly one goroutine should win the SET NX race")
}
