# PubSub Redesign: Constructor-Injected Handler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move `handler func([]byte)` into each PubSub constructor, removing `Subscribe` from the interface and eliminating the handlers-slice + mutex in memory and proxy.

**Architecture:** Each implementation stores a single `handler func([]byte)` set at construction. Memory calls it synchronously in Publish. Proxy's existing `recv` goroutine calls it directly. Redis adds an internal receive goroutine (mirroring proxy's pattern) started in `New`. The `PubSub` interface drops `Subscribe` entirely; only `Publish` and `Close` remain.

**Tech Stack:** Go standard library, `github.com/patrickmn/go-cache`, `github.com/redis/go-redis/v9`, `google.golang.org/grpc`.

---

## File Map

| File | Change |
|------|--------|
| `coordinator/memory/pubsub.go` | Remove `handlers []func([]byte)`, `mu sync.RWMutex`, `Subscribe`. Add `handler func([]byte)` to struct and `New` signature. |
| `coordinator/memory/pubsub_test.go` | Remove `TestPublishCallsAllSubscribers`, `TestSubscribeBlocksUntilContextCancelled`. Rename subscriber tests. Update `New` calls. |
| `coordinator/proxy/pubsub.go` | Remove `handlers []func([]byte)`, `mu sync.RWMutex`, `Subscribe`. Add `handler func([]byte)` to struct and `New` signature. |
| `coordinator/proxy/pubsub_test.go` | Remove Subscribe calls; pass handler to `New`. |
| `coordinator/proxy/integration_test.go` | Use forward-declared vars; pass handler closures to `New`. |
| `coordinator/redis/pubsub.go` | Add `handler func([]byte)`, `ctx`, `cancel`, `done` fields. `New` starts receive goroutine. `Close` cancels and waits. Remove `Subscribe`. |
| `coordinator/redis/pubsub_test.go` | Pass handler to `New`. Remove `Subscribe` goroutine. |
| `coordinator/pubsub.go` | Remove `Subscribe` from interface. |
| `coordinator/main.go` | Forward-declare `srv`. Pass handler closures to each `New`. Remove `go func() { ps.Subscribe(...) }()`. |

---

## Task 1: Update memory.PubSub

**Files:**
- Modify: `coordinator/memory/pubsub_test.go`
- Modify: `coordinator/memory/pubsub.go`

- [ ] **Step 1: Rewrite pubsub_test.go**

Replace the entire file contents:

```go
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
```

- [ ] **Step 2: Verify it fails to compile**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go build ./coordinator/memory/...
```

Expected: compile error — `memory.New` called with wrong number of arguments.

- [ ] **Step 3: Rewrite pubsub.go**

Replace the entire file contents:

```go
package memory

import (
	"context"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

type PubSub struct {
	cache   *gocache.Cache
	handler func([]byte)
}

func New(ttl time.Duration, handler func([]byte)) *PubSub {
	return &PubSub{
		cache:   gocache.New(ttl, ttl*2),
		handler: handler,
	}
}

func (p *PubSub) Publish(_ context.Context, traceID []byte) (bool, error) {
	if p.cache.Add(string(traceID), struct{}{}, gocache.DefaultExpiration) != nil {
		return false, nil
	}
	if p.handler != nil {
		p.handler(traceID)
	}
	return true, nil
}

func (p *PubSub) Close() error { return nil }
```

- [ ] **Step 4: Run tests**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go test ./coordinator/memory/... -v
```

Expected: all tests pass. `TestPublishCallsHandler` and `TestPublishDuplicateDoesNotCallHandler` no longer need `time.Sleep` — handler is called synchronously inside `Publish`.

- [ ] **Step 5: Commit**

```bash
git add coordinator/memory/pubsub.go coordinator/memory/pubsub_test.go
git commit -m "refactor(memory): handler injected via New, drop handlers slice and mutex"
```

---

## Task 2: Update proxy.PubSub

**Files:**
- Modify: `coordinator/proxy/pubsub_test.go`
- Modify: `coordinator/proxy/pubsub.go`
- Modify: `coordinator/proxy/integration_test.go`

- [ ] **Step 1: Rewrite pubsub_test.go**

Replace the entire file contents:

```go
package proxy_test

import (
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"pitr.ca/retroactivesampling/coordinator/proxy"
	gen "pitr.ca/retroactivesampling/proto"
)

type mockServer struct {
	gen.UnimplementedCoordinatorServer
	notified   chan string
	decisionCh chan []byte
	connected  chan struct{}
	once       sync.Once
}

func (m *mockServer) Connect(stream gen.Coordinator_ConnectServer) error {
	m.once.Do(func() { close(m.connected) })
	go func() {
		for {
			select {
			case tid, ok := <-m.decisionCh:
				if !ok {
					return
				}
				_ = stream.Send(&gen.CoordinatorMessage{
					Payload: &gen.CoordinatorMessage_Batch{
						Batch: &gen.BatchTraceDecision{TraceIds: [][]byte{tid}},
					},
				})
			case <-stream.Context().Done():
				return
			}
		}
	}()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			m.notified <- hex.EncodeToString(n.TraceId)
		}
	}
}

func newMock() *mockServer {
	return &mockServer{
		notified:   make(chan string, 10),
		decisionCh: make(chan []byte, 10),
		connected:  make(chan struct{}),
	}
}

func startGRPCServer(t *testing.T, srv gen.CoordinatorServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

const traceHex = "aabbccdd11223344aabbccdd11223344"

var traceBytes, _ = hex.DecodeString(traceHex)

func TestPublishSendsNotifyInteresting(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	ps := proxy.New(addr, nil)
	t.Cleanup(func() { _ = ps.Close() })

	novel, err := ps.Publish(t.Context(), traceBytes)
	require.NoError(t, err)
	assert.False(t, novel)

	select {
	case id := <-mock.notified:
		assert.Equal(t, traceHex, id)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: NotifyInteresting not received by parent coordinator")
	}
}

func TestPublishAlwaysReturnsFalse(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	ps := proxy.New(addr, nil)
	t.Cleanup(func() { _ = ps.Close() })

	for range 3 {
		novel, err := ps.Publish(t.Context(), traceBytes)
		require.NoError(t, err)
		assert.False(t, novel, "proxy.PubSub has no local dedup — novel count tracked by parent coordinator")
	}
}

func TestHandlerFiredOnDecision(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	received := make(chan []byte, 1)
	ps := proxy.New(addr, func(id []byte) { received <- id })
	t.Cleanup(func() { _ = ps.Close() })

	select {
	case <-mock.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy connection not established")
	}

	mock.decisionCh <- traceBytes

	select {
	case id := <-received:
		assert.Equal(t, traceBytes, id)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: handler not called with decision")
	}
}

func TestCloseStopsReconnectLoop(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	_ = lis.Close()

	ps := proxy.New(addr, nil)
	done := make(chan struct{})
	go func() { _ = ps.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not return in time")
	}
}

func TestHandlerNilSafe(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	ps := proxy.New(addr, nil) // nil handler must not panic on decision
	t.Cleanup(func() { _ = ps.Close() })

	select {
	case <-mock.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy connection not established")
	}

	mock.decisionCh <- traceBytes
	time.Sleep(100 * time.Millisecond) // give recv time to call nil handler
	// if we reach here without panic, test passes
}
```

- [ ] **Step 2: Rewrite integration_test.go**

Replace the entire file contents:

```go
package proxy_test

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/server"
	gen "pitr.ca/retroactivesampling/proto"
)

func TestIntegrationLocalToCentral(t *testing.T) {
	// Central coordinator: memory PubSub.
	var centralSrv *server.Server
	memPS := memory.New(time.Minute, func(id []byte) { centralSrv.Broadcast(id) })
	centralSrv = server.New(func(traceID []byte) {
		_, _ = memPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	centralAddr := startGRPCServer(t, centralSrv)

	// Local coordinator: proxy PubSub pointing at central.
	var localSrv *server.Server
	localPS := proxy.New(centralAddr, func(id []byte) { localSrv.Broadcast(id) })
	t.Cleanup(func() { _ = localPS.Close() })
	localSrv = server.New(func(traceID []byte) {
		_, _ = localPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	localAddr := startGRPCServer(t, localSrv)

	// Collector client: connects to local coordinator.
	conn, err := grpc.NewClient(localAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(t.Context())
	require.NoError(t, err)

	traceBytes, _ := hex.DecodeString("aabbccdd11223344aabbccdd11223344")

	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	})
	require.NoError(t, err)

	msg, err := stream.Recv()
	require.NoError(t, err)
	b := msg.GetBatch()
	require.NotNil(t, b)
	require.Len(t, b.TraceIds, 1)
	assert.Equal(t, traceBytes, b.TraceIds[0])
}
```

- [ ] **Step 3: Verify tests fail to compile**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go build ./coordinator/proxy/...
```

Expected: compile error — `proxy.New` called with wrong number of arguments.

- [ ] **Step 4: Rewrite pubsub.go**

Replace the entire file contents:

```go
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "pitr.ca/retroactivesampling/proto"
)

const (
	sendBufSize = 256
	maxBackoff  = 30 * time.Second
)

type PubSub struct {
	conn    *grpc.ClientConn
	sendCh  chan []byte
	handler func([]byte)
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(endpoint string, handler func([]byte)) *PubSub {
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("proxy: new client", "endpoint", endpoint, "err", err)
		os.Exit(1)
	}
	p := &PubSub{
		conn:    conn,
		sendCh:  make(chan []byte, sendBufSize),
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go func() { defer close(p.done); p.run() }()
	return p
}

// Always returns (false, nil) — no local dedup; novel count is tracked by the parent coordinator.
func (p *PubSub) Publish(_ context.Context, traceID []byte) (bool, error) {
	select {
	case p.sendCh <- traceID:
	default:
		slog.Warn("proxy: send buffer full, dropping notify", "trace_id", fmt.Sprintf("%x", traceID))
	}
	return false, nil
}

func (p *PubSub) Close() error {
	p.cancel()
	<-p.done
	return p.conn.Close()
}

func (p *PubSub) run() {
	backoff := time.Second
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		stream, err := gen.NewCoordinatorClient(p.conn).Connect(p.ctx)
		if err != nil {
			slog.Warn("proxy: connect failed, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
			continue
		}
		recvErr := make(chan error, 1)
		go p.recv(stream, recvErr)
		if err := p.send(stream, recvErr); err != nil {
			slog.Warn("proxy: connection lost, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
		} else {
			backoff = time.Second
		}
	}
}

func (p *PubSub) recv(stream gen.Coordinator_ConnectClient, errCh chan<- error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			errCh <- err
			return
		}
		if b := msg.GetBatch(); b != nil && p.handler != nil {
			for _, traceID := range b.TraceIds {
				p.handler(traceID)
			}
		}
	}
}

func (p *PubSub) send(stream gen.Coordinator_ConnectClient, recvErr <-chan error) error {
	for {
		select {
		case traceID := <-p.sendCh:
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: traceID},
				},
			}); err != nil {
				return err
			}
		case err := <-recvErr:
			return err
		case <-p.ctx.Done():
			return nil
		}
	}
}
```

- [ ] **Step 5: Run tests**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go test ./coordinator/proxy/... -v
```

Expected: all tests pass including `TestIntegrationLocalToCentral`.

- [ ] **Step 6: Commit**

```bash
git add coordinator/proxy/pubsub.go coordinator/proxy/pubsub_test.go coordinator/proxy/integration_test.go
git commit -m "refactor(proxy): handler injected via New, drop handlers slice and mutex"
```

---

## Task 3: Update redis.PubSub

**Files:**
- Modify: `coordinator/redis/pubsub_test.go`
- Modify: `coordinator/redis/pubsub.go`

- [ ] **Step 1: Rewrite pubsub_test.go**

Replace the entire file contents:

```go
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
```

- [ ] **Step 2: Verify tests fail to compile**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go build ./coordinator/redis/...
```

Expected: compile error — `redis.New` called with wrong number of arguments.

- [ ] **Step 3: Rewrite pubsub.go**

Replace the entire file contents:

```go
package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	channel       = "retrosampling:interesting"
	decidedKeyFmt = "retrosampling:decided:%x"
)

type PubSub struct {
	writeClient *redis.Client
	readClient  *redis.Client
	ttl         time.Duration
	handler     func([]byte)
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

func New(writeCfg Config, readCfg *Config, ttl time.Duration, handler func([]byte)) (*PubSub, error) {
	writeOpts, err := writeCfg.options()
	if err != nil {
		return nil, err
	}
	wc := redis.NewClient(writeOpts)

	rc := wc
	if readCfg != nil {
		readOpts, err := readCfg.options()
		if err != nil {
			return nil, err
		}
		rc = redis.NewClient(readOpts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &PubSub{
		writeClient: wc,
		readClient:  rc,
		ttl:         ttl,
		handler:     handler,
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	go func() { defer close(p.done); p.receive() }()
	return p, nil
}

// Publish publishes traceID if not already seen (SET NX + PUBLISH). Returns true if newly published.
// Note: SET NX and PUBLISH are not atomic. A coordinator crash between the two operations
// causes silent trace loss — the NX key blocks retries but no broadcast is sent.
// For this system's best-effort semantics this is acceptable.
func (p *PubSub) Publish(ctx context.Context, traceID []byte) (bool, error) {
	key := fmt.Sprintf(decidedKeyFmt, traceID)
	result, err := p.writeClient.SetArgs(ctx, key, 1, redis.SetArgs{TTL: p.ttl, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if result != "OK" {
		return false, nil
	}
	return true, p.writeClient.Publish(ctx, channel, traceID).Err()
}

func (p *PubSub) receive() {
	sub := p.readClient.Subscribe(p.ctx, channel)
	defer func() { _ = sub.Close() }()
	for {
		select {
		case msg := <-sub.Channel():
			if msg != nil && p.handler != nil {
				p.handler([]byte(msg.Payload))
			}
		case <-p.ctx.Done():
			return
		}
	}
}

func (p *PubSub) Close() error {
	p.cancel()
	<-p.done
	err := p.writeClient.Close()
	if p.readClient != p.writeClient {
		if e := p.readClient.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
```

- [ ] **Step 4: Build to verify it compiles**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go build ./coordinator/redis/...
```

Expected: success. (Integration tests require a live Redis; skip running them here.)

- [ ] **Step 5: Commit**

```bash
git add coordinator/redis/pubsub.go coordinator/redis/pubsub_test.go
git commit -m "refactor(redis): handler injected via New, internal receive goroutine"
```

---

## Task 4: Update interface and main.go

**Files:**
- Modify: `coordinator/pubsub.go`
- Modify: `coordinator/main.go`

- [ ] **Step 1: Rewrite pubsub.go**

Replace the entire file contents:

```go
package main

import (
	"context"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/redis"
)

type PubSub interface {
	Publish(ctx context.Context, traceID []byte) (bool, error)
	Close() error
}

var (
	_ PubSub = (*redis.PubSub)(nil)
	_ PubSub = (*memory.PubSub)(nil)
	_ PubSub = (*proxy.PubSub)(nil)
)
```

- [ ] **Step 2: Update main.go**

Make three changes:

**a)** Declare `var srv *server.Server` immediately before the mode switch (currently `var ps PubSub` is declared there):

```go
var ps PubSub
var srv *server.Server
switch m := activeMode.(type) {
```

**b)** Pass handler closures to each `New` in the mode switch:

```go
case *SingleConfig:
    if m.DecidedKeyTTL == 0 {
        fatal("single mode: decided_key_ttl is required")
    }
    slog.Info("running in single-node mode")
    ps = memory.New(m.DecidedKeyTTL, func(id []byte) { srv.Broadcast(id) })
case *DistributedConfig:
    if m.DecidedKeyTTL == 0 {
        fatal("distributed mode: decided_key_ttl is required")
    }
    if m.RedisPrimary.Endpoint == "" {
        fatal("distributed mode: redis_primary.endpoint is required")
    }
    var replicaCfg *redis.Config
    if len(m.RedisReplicas) > 0 {
        rc := m.RedisReplicas[rand.Intn(len(m.RedisReplicas))]
        replicaCfg = &rc
        slog.Info("subscribing to Redis replica", "addr", rc.Endpoint)
    }
    ps, err = redis.New(m.RedisPrimary, replicaCfg, m.DecidedKeyTTL, func(id []byte) { srv.Broadcast(id) })
    if err != nil {
        fatal("redis", "err", err)
    }
case *ProxyConfig:
    if m.Endpoint == "" {
        fatal("proxy mode: endpoint is required")
    }
    slog.Info("running in proxy mode", "endpoint", m.Endpoint)
    ps = proxy.New(m.Endpoint, func(id []byte) { srv.Broadcast(id) })
```

**c)** Replace the `srv := server.New(...)` block and the Subscribe goroutine. Remove `srv :=` (already declared) and remove the `go func() { ps.Subscribe(...) }()` goroutine:

```go
srv = server.New(func(traceID []byte) {
    if ctx.Err() != nil {
        return
    }
    if novel, err := ps.Publish(ctx, traceID); err != nil {
        slog.Error("publish", "trace_id", fmt.Sprintf("%x", traceID), "err", err)
    } else if novel {
        interestingTraces.Add(1)
    }
}, bytesIn, bytesOut, droppedSends, sendErrors)
```

The `go func() { ps.Subscribe(...) }()` block is deleted entirely.

- [ ] **Step 3: Build everything**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go build ./coordinator/...
```

Expected: success with no errors.

- [ ] **Step 4: Run all non-integration tests**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go test ./coordinator/... -v
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add coordinator/pubsub.go coordinator/main.go
git commit -m "refactor(coordinator): remove Subscribe from PubSub interface, wire handlers via constructors"
```
