package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	rds "retroactivesampling/coordinator/redis"
)

func startRedis(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "6379")
	return host + ":" + port.Port()
}

func TestPublishSubscribe(t *testing.T) {
	addr := startRedis(t)
	ps := rds.New(addr, 60*time.Second)
	t.Cleanup(func() { _ = ps.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	received := make(chan string, 1)
	go func() {
		_ = ps.Subscribe(ctx, func(id string) { received <- id })
	}()
	time.Sleep(100 * time.Millisecond) // let subscription establish

	ok, err := ps.Publish(ctx, "trace-abc")
	require.NoError(t, err)
	assert.True(t, ok)

	select {
	case id := <-received:
		assert.Equal(t, "trace-abc", id)
	case <-ctx.Done():
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishDeduplication(t *testing.T) {
	addr := startRedis(t)
	ps := rds.New(addr, 60*time.Second)
	t.Cleanup(func() { _ = ps.Close() })

	ctx := context.Background()

	ok1, err := ps.Publish(ctx, "trace-xyz")
	require.NoError(t, err)
	assert.True(t, ok1, "first publish should succeed")

	ok2, err := ps.Publish(ctx, "trace-xyz")
	require.NoError(t, err)
	assert.False(t, ok2, "duplicate publish should be suppressed")
}
