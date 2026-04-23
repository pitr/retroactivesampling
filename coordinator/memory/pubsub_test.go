package memory_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pitr.ca/retroactivesampling/coordinator/memory"
)

func TestPublishNovel(t *testing.T) {
	ps := memory.New(time.Minute)
	novel, err := ps.Publish(context.Background(), "trace1")
	require.NoError(t, err)
	assert.True(t, novel)
}

func TestPublishDuplicate(t *testing.T) {
	ps := memory.New(time.Minute)
	ctx := context.Background()
	_, _ = ps.Publish(ctx, "trace1")
	novel, err := ps.Publish(ctx, "trace1")
	require.NoError(t, err)
	assert.False(t, novel)
}

func TestPublishCallsSubscriber(t *testing.T) {
	ps := memory.New(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan string, 1)
	go func() { _ = ps.Subscribe(ctx, func(id string) { received <- id }) }()
	time.Sleep(50 * time.Millisecond) // let Subscribe register handler

	_, err := ps.Publish(context.Background(), "trace2")
	require.NoError(t, err)

	select {
	case id := <-received:
		assert.Equal(t, "trace2", id)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber")
	}
}

func TestPublishDuplicateDoesNotCallSubscriber(t *testing.T) {
	ps := memory.New(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	called := 0
	go func() { _ = ps.Subscribe(ctx, func(string) { called++ }) }()
	time.Sleep(50 * time.Millisecond)

	ctx2 := context.Background()
	_, _ = ps.Publish(ctx2, "trace3")
	_, _ = ps.Publish(ctx2, "trace3")

	time.Sleep(50 * time.Millisecond) // give any spurious second call time to arrive
	assert.Equal(t, 1, called)
}

func TestSubscribeBlocksUntilContextCancelled(t *testing.T) {
	ps := memory.New(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_ = ps.Subscribe(ctx, func(string) {})
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
	ctx := context.Background()

	novel, _ := ps.Publish(ctx, "traceX")
	assert.True(t, novel)

	time.Sleep(200 * time.Millisecond) // wait for TTL to expire

	novel, _ = ps.Publish(ctx, "traceX")
	assert.True(t, novel, "should be novel again after TTL expiry")
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
			novel, _ := ps.Publish(ctx, "sametrace")
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
