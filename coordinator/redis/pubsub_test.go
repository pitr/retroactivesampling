//go:build integration

package redis_test

import (
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
	_ = l.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("redis-server", "--port", strconv.Itoa(port), "--loglevel", "warning", "--bind", "127.0.0.1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "redis-server did not start")

	return addr
}

func TestPublishSubscribe(t *testing.T) {
	addr := startRedis(t)

	received := make(chan []byte, 1)
	ps, err := rds.New(rds.Config{Endpoint: addr}, nil, 60*time.Second, func(id []byte) { received <- id })
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	// Wait for the Redis SUBSCRIBE command to be processed server-side before publishing.
	// There is no hook to observe this from outside; 100 ms is well above a localhost round-trip.
	time.Sleep(100 * time.Millisecond)

	traceID := []byte("trace-abc")
	ok, err := ps.Publish(t.Context(), traceID)
	require.NoError(t, err)
	assert.True(t, ok)

	select {
	case id := <-received:
		assert.Equal(t, traceID, id)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishDeduplication(t *testing.T) {
	addr := startRedis(t)
	ps, err := rds.New(rds.Config{Endpoint: addr}, nil, 60*time.Second, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	traceID := []byte("trace-xyz")
	ok1, err := ps.Publish(t.Context(), traceID)
	require.NoError(t, err)
	assert.True(t, ok1, "first publish should succeed")

	ok2, err := ps.Publish(t.Context(), traceID)
	require.NoError(t, err)
	assert.False(t, ok2, "duplicate publish should be suppressed")
}
