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
	ps := memory.New(time.Minute)
	novel, err := ps.Publish(t.Context(), []byte("trace1"))
	require.NoError(t, err)
	assert.True(t, novel)
}

func TestPublishDuplicate(t *testing.T) {
	ps := memory.New(time.Minute)
	ctx := t.Context()
	_, _ = ps.Publish(ctx, []byte("trace1"))
	novel, err := ps.Publish(ctx, []byte("trace1"))
	require.NoError(t, err)
	assert.False(t, novel)
}

func TestPublishCallsSubscriber(t *testing.T) {
	ps := memory.New(time.Minute)

	received := make(chan []byte, 1)
	go func() { _ = ps.Subscribe(t.Context(), func(id []byte) { received <- id }) }()
	time.Sleep(50 * time.Millisecond) // let Subscribe register handler

	_, err := ps.Publish(t.Context(), []byte("trace2"))
	require.NoError(t, err)

	select {
	case id := <-received:
		assert.Equal(t, []byte("trace2"), id)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber")
	}
}

func TestPublishDuplicateDoesNotCallSubscriber(t *testing.T) {
	ps := memory.New(time.Minute)

	var called atomic.Int32
	go func() { _ = ps.Subscribe(t.Context(), func([]byte) { called.Add(1) }) }()
	time.Sleep(50 * time.Millisecond)

	ctx := t.Context()
	_, _ = ps.Publish(ctx, []byte("trace3"))
	_, _ = ps.Publish(ctx, []byte("trace3"))

	time.Sleep(50 * time.Millisecond) // give any spurious second call time to arrive
	assert.Equal(t, int32(1), called.Load())
}

func TestSubscribeBlocksUntilContextCancelled(t *testing.T) {
	ps := memory.New(time.Minute)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		_ = ps.Subscribe(ctx, func([]byte) {})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Subscribe did not return after context cancel")
	}
}

func TestTTLExpiry(t *testing.T) {
	ps := memory.New(100 * time.Millisecond)
	ctx := t.Context()

	novel, _ := ps.Publish(ctx, []byte("traceX"))
	assert.True(t, novel)

	time.Sleep(200 * time.Millisecond) // wait for TTL to expire

	novel, _ = ps.Publish(ctx, []byte("traceX"))
	assert.True(t, novel, "should be novel again after TTL expiry")
}

func TestPublishCallsAllSubscribers(t *testing.T) {
	ps := memory.New(time.Minute)

	r1, r2 := make(chan []byte, 1), make(chan []byte, 1)
	go func() { _ = ps.Subscribe(t.Context(), func(id []byte) { r1 <- id }) }()
	go func() { _ = ps.Subscribe(t.Context(), func(id []byte) { r2 <- id }) }()
	time.Sleep(50 * time.Millisecond)

	_, err := ps.Publish(t.Context(), []byte("trace4"))
	require.NoError(t, err)

	for _, ch := range []chan []byte{r1, r2} {
		select {
		case id := <-ch:
			assert.Equal(t, []byte("trace4"), id)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for subscriber")
		}
	}
}

func TestConcurrentPublish(t *testing.T) {
	ps := memory.New(time.Minute)
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
