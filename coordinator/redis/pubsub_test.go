//go:build integration

package redis_test

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rds "pitr.ca/retroactivesampling/coordinator/redis"
)

func startRedis(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("redis-server", "--port", strconv.Itoa(port), "--loglevel", "warning", "--bind", "127.0.0.1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "redis-server did not start")

	return addr
}

func TestPublishSubscribe(t *testing.T) {
	addr := startRedis(t)
	ps, err := rds.New(rds.Config{Endpoint: addr}, 60*time.Second)
	require.NoError(t, err)
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
	ps, err := rds.New(rds.Config{Endpoint: addr}, 60*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	ctx := context.Background()

	ok1, err := ps.Publish(ctx, "trace-xyz")
	require.NoError(t, err)
	assert.True(t, ok1, "first publish should succeed")

	ok2, err := ps.Publish(ctx, "trace-xyz")
	require.NoError(t, err)
	assert.False(t, ok2, "duplicate publish should be suppressed")
}
