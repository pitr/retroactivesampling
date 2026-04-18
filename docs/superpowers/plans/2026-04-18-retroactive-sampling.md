# Retroactive Sampling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an otelcol processor that retroactively samples traces using a shared coordinator service backed by Redis pub/sub.

**Architecture:** Spans arrive at the processor, are buffered in BBolt keyed by trace ID, and evaluated after `buffer_ttl` of inactivity. Interesting traces are ingested immediately and broadcast via a coordinator gRPC service; non-interesting traces wait for a coordinator push or are dropped after `drop_ttl`. Coordinators share state through Redis pub/sub and use a `SET NX` dedup key to avoid duplicate broadcasts.

**Tech Stack:** Go 1.22, BBolt (embedded KV), gRPC + protobuf, Redis (`go-redis/v9`), OpenTelemetry Collector SDK (`pdata`, `component`, `processor`, `consumer`), testcontainers-go (integration tests), testify (assertions).

---

## File Map

```
retroactivesampling/
├── go.mod
├── proto/
│   └── coordinator.proto
├── gen/                                  # generated — do not edit
│   ├── coordinator.pb.go
│   └── coordinator_grpc.pb.go
├── processor/
│   ├── config.go                         # Config struct + defaults
│   ├── factory.go                        # otelcol factory
│   ├── processor.go                      # ConsumeTraces, timer maps, onDecision
│   ├── processor_test.go                 # integration test (fake coordinator)
│   ├── split.go                          # groupByTrace helper
│   ├── buffer/
│   │   ├── buffer.go                     # SpanBuffer (BBolt, two buckets)
│   │   └── buffer_test.go
│   ├── evaluator/
│   │   ├── evaluator.go                  # Evaluator interface + Chain
│   │   ├── error.go                      # ErrorEvaluator
│   │   ├── error_test.go
│   │   ├── latency.go                    # LatencyEvaluator
│   │   ├── latency_test.go
│   │   └── registry.go                   # Build([]RuleConfig) Chain
│   ├── cache/
│   │   ├── cache.go                      # InterestCache (sync.Mutex + TTL)
│   │   └── cache_test.go
│   └── coordinator/
│       ├── client.go                     # gRPC client, reconnect loop
│       └── client_test.go               # fake gRPC server
└── coordinator/
    ├── main.go                           # wire-up + config loading
    ├── config.go                         # Config struct
    ├── server/
    │   ├── server.go                     # gRPC Connect impl, processor registry
    │   └── server_test.go
    └── redis/
        ├── pubsub.go                     # Publish (SET NX + PUBLISH), Subscribe
        └── pubsub_test.go               # testcontainers Redis
```

---

## Phase 1 — Foundation

### Task 1: Module setup

**Files:**
- Create: `go.mod`
- Create: `proto/coordinator.proto`

- [ ] **Step 1: Initialise module**

```bash
cd /path/to/retroactivesampling
go mod init retroactivesampling
```

Expected: `go.mod` created with `module retroactivesampling`.

- [ ] **Step 2: Pull dependencies**

```bash
go get go.etcd.io/bbolt@latest
go get github.com/redis/go-redis/v9@latest
go get google.golang.org/grpc@latest
go get google.golang.org/protobuf@latest
go get go.opentelemetry.io/collector/pdata@latest
go get go.opentelemetry.io/collector/component@latest
go get go.opentelemetry.io/collector/consumer@latest
go get go.opentelemetry.io/collector/processor@latest
go get go.opentelemetry.io/collector/confmap@latest
go get github.com/stretchr/testify@latest
go get github.com/testcontainers/testcontainers-go@latest
go get go.uber.org/zap@latest
```

- [ ] **Step 3: Install protoc plugins**

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

Verify `protoc` is available: `protoc --version` (install via `brew install protobuf` if missing).

- [ ] **Step 4: Write proto**

Create `proto/coordinator.proto`:

```protobuf
syntax = "proto3";
package coordinator;
option go_package = "retroactivesampling/gen";

service Coordinator {
  rpc Connect(stream ProcessorMessage) returns (stream CoordinatorMessage);
}

message ProcessorMessage {
  oneof payload {
    NotifyInteresting notify = 1;
  }
}

message CoordinatorMessage {
  oneof payload {
    TraceDecision decision = 1;
  }
}

message NotifyInteresting {
  string trace_id = 1;
}

message TraceDecision {
  string trace_id = 1;
  bool keep = 2;
}
```

- [ ] **Step 5: Generate gRPC code**

```bash
mkdir -p gen
protoc --go_out=gen --go_opt=paths=source_relative \
       --go-grpc_out=gen --go-grpc_opt=paths=source_relative \
       -I proto proto/coordinator.proto
```

Expected: `gen/coordinator.pb.go` and `gen/coordinator_grpc.pb.go` created.

- [ ] **Step 6: Verify build**

```bash
go build ./...
```

Expected: no errors (gen package compiles).

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum proto/ gen/
git commit -m "feat: module setup and proto definition"
```

---

## Phase 2 — Coordinator

### Task 2: Redis pubsub

**Files:**
- Create: `coordinator/redis/pubsub.go`
- Create: `coordinator/redis/pubsub_test.go`

- [ ] **Step 1: Write the failing test**

Create `coordinator/redis/pubsub_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./coordinator/redis/... -v -run TestPublish
```

Expected: FAIL — `rds` package does not exist.

- [ ] **Step 3: Implement pubsub**

Create `coordinator/redis/pubsub.go`:

```go
package redis

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	channel         = "retrosampling:interesting"
	decidedKeyFmt   = "retrosampling:decided:%s"
)

type PubSub struct {
	client *redis.Client
	ttl    time.Duration
}

func New(addr string, ttl time.Duration) *PubSub {
	return &PubSub{
		client: redis.NewClient(&redis.Options{Addr: addr}),
		ttl:    ttl,
	}
}

// Publish publishes traceID if not already seen (SET NX). Returns true if newly published.
func (p *PubSub) Publish(ctx context.Context, traceID string) (bool, error) {
	key := fmt.Sprintf(decidedKeyFmt, traceID)
	ok, err := p.client.SetNX(ctx, key, 1, p.ttl).Result()
	if err != nil || !ok {
		return false, err
	}
	return true, p.client.Publish(ctx, channel, traceID).Err()
}

// Subscribe calls handler for each traceID received. Blocks until ctx is cancelled.
func (p *PubSub) Subscribe(ctx context.Context, handler func(traceID string)) error {
	sub := p.client.Subscribe(ctx, channel)
	defer sub.Close()
	for {
		select {
		case msg := <-sub.Channel():
			handler(msg.Payload)
		case <-ctx.Done():
			return nil
		}
	}
}

func (p *PubSub) Close() error {
	return p.client.Close()
}
```

Add missing import `"fmt"` to the file.

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./coordinator/redis/... -v -run TestPublish
```

Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add coordinator/redis/
git commit -m "feat: coordinator Redis pubsub with SET NX dedup"
```

---

### Task 3: Coordinator gRPC server

**Files:**
- Create: `coordinator/server/server.go`
- Create: `coordinator/server/server_test.go`

- [ ] **Step 1: Write the failing test**

Create `coordinator/server/server_test.go`:

```go
package server_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "retroactivesampling/gen"
	"retroactivesampling/coordinator/server"
)

func startServer(t *testing.T, s *server.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func TestBroadcastToConnectedProcessors(t *testing.T) {
	notified := make(chan string, 1)
	srv := server.New(func(traceID string) { notified <- traceID })
	addr := startServer(t, srv)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(context.Background())
	require.NoError(t, err)

	// Processor sends notification
	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: "trace-1"},
		},
	})
	require.NoError(t, err)

	select {
	case id := <-notified:
		assert.Equal(t, "trace-1", id)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
	}

	// Coordinator broadcasts back
	srv.Broadcast("trace-1", true)

	msg, err := stream.Recv()
	require.NoError(t, err)
	d := msg.GetDecision()
	require.NotNil(t, d)
	assert.Equal(t, "trace-1", d.TraceId)
	assert.True(t, d.Keep)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./coordinator/server/... -v -run TestBroadcast
```

Expected: FAIL — `server` package does not exist.

- [ ] **Step 3: Implement the server**

Create `coordinator/server/server.go`:

```go
package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"

	gen "retroactivesampling/gen"
)

type Server struct {
	gen.UnimplementedCoordinatorServer
	mu       sync.RWMutex
	streams  map[string]gen.Coordinator_ConnectServer
	onNotify func(traceID string)
}

func New(onNotify func(traceID string)) *Server {
	return &Server{
		streams:  make(map[string]gen.Coordinator_ConnectServer),
		onNotify: onNotify,
	}
}

func (s *Server) Connect(stream gen.Coordinator_ConnectServer) error {
	id := randomID()
	s.mu.Lock()
	s.streams[id] = stream
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}()

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			s.onNotify(n.TraceId)
		}
	}
}

// Broadcast sends keep decision to all connected processors. Best-effort.
func (s *Server) Broadcast(traceID string, keep bool) {
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: traceID, Keep: keep},
		},
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, stream := range s.streams {
		_ = stream.Send(msg)
	}
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./coordinator/server/... -v -run TestBroadcast
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add coordinator/server/
git commit -m "feat: coordinator gRPC server with processor registry"
```

---

### Task 4: Coordinator main

**Files:**
- Create: `coordinator/config.go`
- Create: `coordinator/main.go`

- [ ] **Step 1: Write config**

Create `coordinator/config.go`:

```go
package main

import (
	"time"

	"gopkg.in/yaml.v3"
	"os"
)

type Config struct {
	GRPCListen     string        `yaml:"grpc_listen"`
	RedisAddr      string        `yaml:"redis_addr"`
	DecidedKeyTTL  time.Duration `yaml:"decided_key_ttl"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	return &cfg, yaml.Unmarshal(data, &cfg)
}
```

Add yaml dependency: `go get gopkg.in/yaml.v3@latest`.

- [ ] **Step 2: Write main**

Create `coordinator/main.go`:

```go
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	gen "retroactivesampling/gen"
	rds "retroactivesampling/coordinator/redis"
	"retroactivesampling/coordinator/server"
)

func main() {
	cfgPath := flag.String("config", "coordinator.yaml", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ps := rds.New(cfg.RedisAddr, cfg.DecidedKeyTTL)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := server.New(func(traceID string) {
		ok, err := ps.Publish(ctx, traceID)
		if err != nil {
			log.Printf("publish error: %v", err)
			return
		}
		if !ok {
			return // duplicate, already broadcast by another instance
		}
	})

	// Subscribe to Redis; broadcast decisions from other coordinator instances
	go func() {
		_ = ps.Subscribe(ctx, func(traceID string) {
			srv.Broadcast(traceID, true)
		})
	}()

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)

	go func() {
		<-ctx.Done()
		gs.GracefulStop()
		_ = ps.Close()
	}()

	log.Printf("coordinator listening on %s", cfg.GRPCListen)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
```

- [ ] **Step 3: Build coordinator binary**

```bash
go build -o bin/coordinator ./coordinator/
```

Expected: `bin/coordinator` produced, no errors.

- [ ] **Step 4: Commit**

```bash
git add coordinator/config.go coordinator/main.go
git commit -m "feat: coordinator main with Redis pub/sub wiring"
```

---

## Phase 3 — Processor Building Blocks

### Task 5: SpanBuffer (BBolt)

**Files:**
- Create: `processor/buffer/buffer.go`
- Create: `processor/buffer/buffer_test.go`

- [ ] **Step 1: Write the failing tests**

Create `processor/buffer/buffer_test.go`:

```go
package buffer_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"retroactivesampling/processor/buffer"
)

func newTestBuffer(t *testing.T) *buffer.SpanBuffer {
	t.Helper()
	f, err := os.CreateTemp("", "buffer-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	buf, err := buffer.New(f.Name())
	require.NoError(t, err)
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func singleSpanTraces(traceID string, statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	var tid [16]byte
	copy(tid[:], traceID)
	span.SetTraceID(tid)
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

func TestWriteAndRead(t *testing.T) {
	buf := newTestBuffer(t)
	traces := singleSpanTraces("trace1_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 100)

	require.NoError(t, buf.Write("trace1", traces, time.Now()))

	got, ok, err := buf.Read("trace1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.SpanCount())
}

func TestWriteAppendsSpans(t *testing.T) {
	buf := newTestBuffer(t)
	t1 := singleSpanTraces("trace2_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 50)
	t2 := singleSpanTraces("trace2_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 60)

	require.NoError(t, buf.Write("trace2", t1, time.Now()))
	require.NoError(t, buf.Write("trace2", t2, time.Now()))

	got, ok, err := buf.Read("trace2")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.SpanCount())
}

func TestDelete(t *testing.T) {
	buf := newTestBuffer(t)
	traces := singleSpanTraces("trace3_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 100)

	require.NoError(t, buf.Write("trace3", traces, time.Now()))
	require.NoError(t, buf.Delete("trace3"))

	_, ok, err := buf.Read("trace3")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestEvictOldestFIFO(t *testing.T) {
	buf := newTestBuffer(t)
	now := time.Now()
	t1 := singleSpanTraces("traceA_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)
	t2 := singleSpanTraces("traceB_xxxxxxxxxxxxxA", ptrace.StatusCodeOk, 10)

	require.NoError(t, buf.Write("traceA", t1, now))
	require.NoError(t, buf.Write("traceB", t2, now.Add(time.Millisecond)))

	evicted, err := buf.EvictOldest()
	require.NoError(t, err)
	assert.Equal(t, "traceA", evicted, "oldest trace should be evicted first")

	_, ok, _ := buf.Read("traceA")
	assert.False(t, ok, "evicted trace should be gone")

	_, ok, _ = buf.Read("traceB")
	assert.True(t, ok, "newer trace should remain")
}
```

Note: import `"go.opentelemetry.io/collector/pdata/pcommon"` for `pcommon.NewTimestampFromTime`.

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./processor/buffer/... -v
```

Expected: FAIL — `buffer` package does not exist.

- [ ] **Step 3: Implement SpanBuffer**

Create `processor/buffer/buffer.go`:

```go
package buffer

import (
	"encoding/binary"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

var (
	tracesBucket = []byte("traces")
	orderBucket  = []byte("order")
)

type SpanBuffer struct {
	db *bolt.DB
}

func New(path string) (*SpanBuffer, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(tracesBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(orderBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &SpanBuffer{db: db}, nil
}

func (b *SpanBuffer) Close() error { return b.db.Close() }

// orderKey produces a big-endian timestamp prefix + traceID for FIFO cursor ordering.
func orderKey(ts time.Time, traceID string) []byte {
	key := make([]byte, 8+len(traceID))
	binary.BigEndian.PutUint64(key[:8], uint64(ts.UnixNano()))
	copy(key[8:], traceID)
	return key
}

// Write buffers spans for traceID. First write records insertion time in the order bucket.
// Subsequent writes for the same traceID append spans (preserving original insertion order).
// Value layout in traces bucket: [8 bytes insertion_ns][otlp proto bytes]
func (b *SpanBuffer) Write(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(spans)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	return b.db.Update(func(tx *bolt.Tx) error {
		tb := tx.Bucket(tracesBucket)
		ob := tx.Bucket(orderBucket)
		key := []byte(traceID)
		existing := tb.Get(key)

		if existing == nil {
			// New trace: record in order bucket with insertion timestamp as header
			if err := ob.Put(orderKey(insertedAt, traceID), nil); err != nil {
				return err
			}
			var hdr [8]byte
			binary.BigEndian.PutUint64(hdr[:], uint64(insertedAt.UnixNano()))
			return tb.Put(key, append(hdr[:], data...))
		}

		// Existing trace: keep original insertion timestamp, merge spans
		u := ptrace.ProtoUnmarshaler{}
		prev, err := u.UnmarshalTraces(existing[8:])
		if err != nil {
			return fmt.Errorf("unmarshal existing: %w", err)
		}
		spans.ResourceSpans().MoveAndAppendTo(prev.ResourceSpans())
		merged, err := m.MarshalTraces(prev)
		if err != nil {
			return fmt.Errorf("marshal merged: %w", err)
		}
		return tb.Put(key, append(existing[:8:8], merged...))
	})
}

// Read retrieves all buffered spans for traceID. ok=false means trace not found.
func (b *SpanBuffer) Read(traceID string) (ptrace.Traces, bool, error) {
	var data []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(tracesBucket).Get([]byte(traceID))
		if v != nil && len(v) > 8 {
			data = make([]byte, len(v)-8)
			copy(data, v[8:])
		}
		return nil
	})
	if err != nil || data == nil {
		return ptrace.Traces{}, false, err
	}
	u := ptrace.ProtoUnmarshaler{}
	t, err := u.UnmarshalTraces(data)
	return t, err == nil, err
}

// Delete removes a trace from both buckets atomically.
func (b *SpanBuffer) Delete(traceID string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		tb := tx.Bucket(tracesBucket)
		key := []byte(traceID)
		v := tb.Get(key)
		if v == nil {
			return nil
		}
		tsNs := binary.BigEndian.Uint64(v[:8])
		if err := tx.Bucket(orderBucket).Delete(orderKey(time.Unix(0, int64(tsNs)), traceID)); err != nil {
			return err
		}
		return tb.Delete(key)
	})
}

// EvictOldest removes the oldest trace (by insertion time) to free space.
// Returns the evicted traceID, or "" if the buffer is empty.
func (b *SpanBuffer) EvictOldest() (string, error) {
	var evicted string
	err := b.db.Update(func(tx *bolt.Tx) error {
		ob := tx.Bucket(orderBucket)
		c := ob.Cursor()
		k, _ := c.First()
		if k == nil {
			return nil
		}
		traceID := string(k[8:])
		evicted = traceID
		if err := ob.Delete(k); err != nil {
			return err
		}
		return tx.Bucket(tracesBucket).Delete([]byte(traceID))
	})
	return evicted, err
}

// WriteWithEviction writes spans, evicting oldest traces if the disk is full.
func (b *SpanBuffer) WriteWithEviction(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
	for {
		err := b.Write(traceID, spans, insertedAt)
		if err == nil {
			return nil
		}
		if !isDiskFull(err) {
			return err
		}
		if _, evictErr := b.EvictOldest(); evictErr != nil {
			return evictErr
		}
	}
}

func isDiskFull(err error) bool {
	return strings.Contains(err.Error(), "no space left")
}
```

Add `"strings"` to the imports.

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./processor/buffer/... -v
```

Expected: all 4 tests pass.

- [ ] **Step 5: Commit**

```bash
git add processor/buffer/
git commit -m "feat: BBolt-backed SpanBuffer with FIFO eviction"
```

---

### Task 6: InterestCache

**Files:**
- Create: `processor/cache/cache.go`
- Create: `processor/cache/cache_test.go`

- [ ] **Step 1: Write the failing tests**

Create `processor/cache/cache_test.go`:

```go
package cache_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"retroactivesampling/processor/cache"
)

func TestHasReturnsTrueAfterAdd(t *testing.T) {
	c := cache.New(time.Minute)
	c.Add("trace-1")
	assert.True(t, c.Has("trace-1"))
}

func TestHasReturnsFalseForUnknown(t *testing.T) {
	c := cache.New(time.Minute)
	assert.False(t, c.Has("trace-unknown"))
}

func TestHasReturnsFalseAfterExpiry(t *testing.T) {
	c := cache.New(50 * time.Millisecond)
	c.Add("trace-2")
	time.Sleep(80 * time.Millisecond)
	assert.False(t, c.Has("trace-2"))
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./processor/cache/... -v
```

Expected: FAIL — `cache` package does not exist.

- [ ] **Step 3: Implement InterestCache**

Create `processor/cache/cache.go`:

```go
package cache

import (
	"sync"
	"time"
)

type InterestCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
}

func New(ttl time.Duration) *InterestCache {
	c := &InterestCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
	}
	go c.sweep()
	return c
}

func (c *InterestCache) Add(traceID string) {
	c.mu.Lock()
	c.entries[traceID] = time.Now().Add(c.ttl)
	c.mu.Unlock()
}

func (c *InterestCache) Has(traceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[traceID]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(c.entries, traceID)
		return false
	}
	return true
}

func (c *InterestCache) sweep() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		c.mu.Lock()
		for id, exp := range c.entries {
			if now.After(exp) {
				delete(c.entries, id)
			}
		}
		c.mu.Unlock()
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./processor/cache/... -v
```

Expected: all 3 tests pass.

- [ ] **Step 5: Commit**

```bash
git add processor/cache/
git commit -m "feat: InterestCache with TTL expiry"
```

---

### Task 7: Evaluators

**Files:**
- Create: `processor/evaluator/evaluator.go`
- Create: `processor/evaluator/error.go`
- Create: `processor/evaluator/error_test.go`
- Create: `processor/evaluator/latency.go`
- Create: `processor/evaluator/latency_test.go`
- Create: `processor/evaluator/registry.go`

- [ ] **Step 1: Write interface + chain**

Create `processor/evaluator/evaluator.go`:

```go
package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type Evaluator interface {
	Evaluate(ptrace.Traces) bool
}

// Chain evaluates each Evaluator in order; returns true on first match (OR logic).
type Chain []Evaluator

func (c Chain) Evaluate(t ptrace.Traces) bool {
	for _, e := range c {
		if e.Evaluate(t) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Write failing tests for ErrorEvaluator**

Create `processor/evaluator/error_test.go`:

```go
package evaluator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"retroactivesampling/processor/evaluator"
)

func makeTraces(statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

func TestErrorEvaluator_ErrorSpan(t *testing.T) {
	e := &evaluator.ErrorEvaluator{}
	assert.True(t, e.Evaluate(makeTraces(ptrace.StatusCodeError, 10)))
}

func TestErrorEvaluator_OkSpan(t *testing.T) {
	e := &evaluator.ErrorEvaluator{}
	assert.False(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 10)))
}
```

- [ ] **Step 3: Run test to verify it fails**

```bash
go test ./processor/evaluator/... -v -run TestError
```

Expected: FAIL — `evaluator` package does not exist.

- [ ] **Step 4: Implement ErrorEvaluator**

Create `processor/evaluator/error.go`:

```go
package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type ErrorEvaluator struct{}

func (e *ErrorEvaluator) Evaluate(t ptrace.Traces) bool {
	for i := 0; i < t.ResourceSpans().Len(); i++ {
		for j := 0; j < t.ResourceSpans().At(i).ScopeSpans().Len(); j++ {
			ss := t.ResourceSpans().At(i).ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				if ss.Spans().At(k).Status().Code() == ptrace.StatusCodeError {
					return true
				}
			}
		}
	}
	return false
}
```

- [ ] **Step 5: Write failing tests for LatencyEvaluator**

Create `processor/evaluator/latency_test.go`:

```go
package evaluator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"retroactivesampling/processor/evaluator"
)

func TestLatencyEvaluator_AboveThreshold(t *testing.T) {
	e := &evaluator.LatencyEvaluator{Threshold: 5 * time.Second}
	assert.True(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 6000)))
}

func TestLatencyEvaluator_BelowThreshold(t *testing.T) {
	e := &evaluator.LatencyEvaluator{Threshold: 5 * time.Second}
	assert.False(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 1000)))
}

func TestLatencyEvaluator_AtThreshold(t *testing.T) {
	e := &evaluator.LatencyEvaluator{Threshold: 5 * time.Second}
	assert.True(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 5000)))
}
```

Add missing `"go.opentelemetry.io/collector/pdata/ptrace"` import to `latency_test.go`.

- [ ] **Step 6: Implement LatencyEvaluator**

Create `processor/evaluator/latency.go`:

```go
package evaluator

import (
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

type LatencyEvaluator struct {
	Threshold time.Duration
}

func (e *LatencyEvaluator) Evaluate(t ptrace.Traces) bool {
	for i := 0; i < t.ResourceSpans().Len(); i++ {
		for j := 0; j < t.ResourceSpans().At(i).ScopeSpans().Len(); j++ {
			ss := t.ResourceSpans().At(i).ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				d := span.EndTimestamp().AsTime().Sub(span.StartTimestamp().AsTime())
				if d >= e.Threshold {
					return true
				}
			}
		}
	}
	return false
}
```

- [ ] **Step 7: Implement registry**

Create `processor/evaluator/registry.go`:

```go
package evaluator

import (
	"fmt"
	"time"
)

type RuleConfig struct {
	Type      string        `mapstructure:"type"`
	Threshold time.Duration `mapstructure:"threshold"`
}

func Build(rules []RuleConfig) (Chain, error) {
	chain := make(Chain, 0, len(rules))
	for _, r := range rules {
		switch r.Type {
		case "error_status":
			chain = append(chain, &ErrorEvaluator{})
		case "high_latency":
			if r.Threshold == 0 {
				return nil, fmt.Errorf("high_latency rule requires threshold")
			}
			chain = append(chain, &LatencyEvaluator{Threshold: r.Threshold})
		default:
			return nil, fmt.Errorf("unknown rule type: %s", r.Type)
		}
	}
	return chain, nil
}
```

- [ ] **Step 8: Run all evaluator tests**

```bash
go test ./processor/evaluator/... -v
```

Expected: all 5 tests pass.

- [ ] **Step 9: Commit**

```bash
git add processor/evaluator/
git commit -m "feat: pluggable evaluator chain (error_status, high_latency)"
```

---

### Task 8: Coordinator gRPC client

**Files:**
- Create: `processor/coordinator/client.go`
- Create: `processor/coordinator/client_test.go`

- [ ] **Step 1: Write the failing test**

Create `processor/coordinator/client_test.go`:

```go
package coordinator_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	gen "retroactivesampling/gen"
	coord "retroactivesampling/processor/coordinator"
)

// fakeServer records notifications and allows broadcasting decisions.
type fakeServer struct {
	gen.UnimplementedCoordinatorServer
	mu        sync.Mutex
	notified  []string
	streams   []gen.Coordinator_ConnectServer
}

func (f *fakeServer) Connect(stream gen.Coordinator_ConnectServer) error {
	f.mu.Lock()
	f.streams = append(f.streams, stream)
	f.mu.Unlock()
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			f.mu.Lock()
			f.notified = append(f.notified, n.TraceId)
			f.mu.Unlock()
		}
	}
}

func (f *fakeServer) broadcast(traceID string) {
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: traceID, Keep: true},
		},
	}
	f.mu.Lock()
	for _, s := range f.streams {
		_ = s.Send(msg)
	}
	f.mu.Unlock()
}

func startFakeServer(t *testing.T) (*fakeServer, string) {
	t.Helper()
	srv := &fakeServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return srv, lis.Addr().String()
}

func TestClientSendsNotification(t *testing.T) {
	srv, addr := startFakeServer(t)
	received := make(chan string, 1)
	c := coord.New(addr, func(traceID string, keep bool) { received <- traceID })
	t.Cleanup(c.Close)

	time.Sleep(100 * time.Millisecond) // connection establishment
	c.Notify("trace-99")
	time.Sleep(100 * time.Millisecond)

	srv.mu.Lock()
	notified := srv.notified
	srv.mu.Unlock()
	require.Contains(t, notified, "trace-99")
}

func TestClientReceivesDecision(t *testing.T) {
	srv, addr := startFakeServer(t)
	received := make(chan string, 1)
	c := coord.New(addr, func(traceID string, keep bool) {
		if keep {
			received <- traceID
		}
	})
	t.Cleanup(c.Close)

	time.Sleep(100 * time.Millisecond)
	srv.broadcast("trace-55")

	select {
	case id := <-received:
		assert.Equal(t, "trace-55", id)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for decision")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./processor/coordinator/... -v
```

Expected: FAIL — `coordinator` package does not exist.

- [ ] **Step 3: Implement the client**

Create `processor/coordinator/client.go`:

```go
package coordinator

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "retroactivesampling/gen"
)

type DecisionHandler func(traceID string, keep bool)

type Client struct {
	endpoint string
	handler  DecisionHandler
	sendCh   chan string
	ctx      context.Context
	cancel   context.CancelFunc
}

func New(endpoint string, handler DecisionHandler) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		endpoint: endpoint,
		handler:  handler,
		sendCh:   make(chan string, 256),
		ctx:      ctx,
		cancel:   cancel,
	}
	go c.run()
	return c
}

func (c *Client) Notify(traceID string) {
	select {
	case c.sendCh <- traceID:
	default: // drop if backpressure
	}
}

func (c *Client) Close() { c.cancel() }

func (c *Client) run() {
	backoff := time.Second
	for {
		if err := c.connect(); err != nil {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		} else {
			backoff = time.Second
		}
	}
}

func (c *Client) connect() error {
	conn, err := grpc.NewClient(c.endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := gen.NewCoordinatorClient(conn).Connect(c.ctx)
	if err != nil {
		return err
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if d := msg.GetDecision(); d != nil {
				c.handler(d.TraceId, d.Keep)
			}
		}
	}()

	for {
		select {
		case traceID := <-c.sendCh:
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: traceID},
				},
			}); err != nil {
				return err
			}
		case err := <-recvErr:
			return err
		case <-c.ctx.Done():
			return nil
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./processor/coordinator/... -v
```

Expected: both tests pass.

- [ ] **Step 5: Commit**

```bash
git add processor/coordinator/
git commit -m "feat: processor gRPC client with reconnect backoff"
```

---

## Phase 4 — Processor Wiring

### Task 9: Factory + config

**Files:**
- Create: `processor/config.go`
- Create: `processor/factory.go`

- [ ] **Step 1: Write config**

Create `processor/config.go`:

```go
package processor

import (
	"time"

	"retroactivesampling/processor/evaluator"
)

type Config struct {
	BufferDBPath        string                 `mapstructure:"buffer_db_path"`
	BufferTTL           time.Duration          `mapstructure:"buffer_ttl"`
	DropTTL             time.Duration          `mapstructure:"drop_ttl"`
	InterestCacheTTL    time.Duration          `mapstructure:"interest_cache_ttl"`
	CoordinatorEndpoint string                 `mapstructure:"coordinator_endpoint"`
	Rules               []evaluator.RuleConfig `mapstructure:"rules"`
}
```

- [ ] **Step 2: Write factory**

Create `processor/factory.go`:

```go
package processor

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	otelprocessor "go.opentelemetry.io/collector/processor"
)

const typeStr = "retroactive_sampling"

func NewFactory() otelprocessor.Factory {
	return otelprocessor.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		otelprocessor.WithTraces(createTracesProcessor, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		BufferTTL:        10 * time.Second,
		DropTTL:          30 * time.Second,
		InterestCacheTTL: 60 * time.Second,
	}
}

func createTracesProcessor(
	ctx context.Context,
	set otelprocessor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (otelprocessor.Traces, error) {
	return newProcessor(set.Logger, cfg.(*Config), next)
}
```

- [ ] **Step 3: Verify build**

```bash
go build ./processor/...
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add processor/config.go processor/factory.go
git commit -m "feat: processor otelcol factory and config"
```

---

### Task 10: ConsumeTraces core

**Files:**
- Create: `processor/split.go`
- Create: `processor/processor.go`
- Create: `processor/processor_test.go`

- [ ] **Step 1: Write groupByTrace helper**

Create `processor/split.go`:

```go
package processor

import "go.opentelemetry.io/collector/pdata/ptrace"

// groupByTrace splits a ptrace.Traces into per-trace-ID sub-Traces, preserving
// ResourceSpans and ScopeSpans structure.
func groupByTrace(td ptrace.Traces) map[string]ptrace.Traces {
	out := make(map[string]ptrace.Traces)
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				tid := span.TraceID().String()

				t, ok := out[tid]
				if !ok {
					t = ptrace.NewTraces()
					out[tid] = t
				}

				// Find or create matching ResourceSpans
				newRS := findResourceSpans(t, rs)
				// Find or create matching ScopeSpans
				newSS := findScopeSpans(newRS, ss)
				span.CopyTo(newSS.Spans().AppendEmpty())
			}
		}
	}
	return out
}

func findResourceSpans(t ptrace.Traces, src ptrace.ResourceSpans) ptrace.ResourceSpans {
	for i := 0; i < t.ResourceSpans().Len(); i++ {
		rs := t.ResourceSpans().At(i)
		if rs.Resource().Attributes().Equal(src.Resource().Attributes()) {
			return rs
		}
	}
	rs := t.ResourceSpans().AppendEmpty()
	src.Resource().CopyTo(rs.Resource())
	rs.SetSchemaUrl(src.SchemaUrl())
	return rs
}

func findScopeSpans(rs ptrace.ResourceSpans, src ptrace.ScopeSpans) ptrace.ScopeSpans {
	for i := 0; i < rs.ScopeSpans().Len(); i++ {
		ss := rs.ScopeSpans().At(i)
		if ss.Scope().Name() == src.Scope().Name() && ss.Scope().Version() == src.Scope().Version() {
			return ss
		}
	}
	ss := rs.ScopeSpans().AppendEmpty()
	src.Scope().CopyTo(ss.Scope())
	ss.SetSchemaUrl(src.SchemaUrl())
	return ss
}
```

- [ ] **Step 2: Write the failing integration test**

Create `processor/processor_test.go`:

```go
package processor_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	otelprocessor "go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"

	gen "retroactivesampling/gen"
	"retroactivesampling/processor"
	"retroactivesampling/processor/evaluator"
)

// fakeCoordinator records notifications and allows triggering decisions.
type fakeCoordinator struct {
	gen.UnimplementedCoordinatorServer
	mu       sync.Mutex
	notified []string
	streams  []gen.Coordinator_ConnectServer
}

func (f *fakeCoordinator) Connect(stream gen.Coordinator_ConnectServer) error {
	f.mu.Lock()
	f.streams = append(f.streams, stream)
	f.mu.Unlock()
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			f.mu.Lock()
			f.notified = append(f.notified, n.TraceId)
			f.mu.Unlock()
		}
	}
}

func (f *fakeCoordinator) sendDecision(traceID string, keep bool) {
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: traceID, Keep: keep},
		},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.streams {
		_ = s.Send(msg)
	}
}

func startFakeCoordinator(t *testing.T) (*fakeCoordinator, string) {
	t.Helper()
	fc := &fakeCoordinator{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, fc)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return fc, lis.Addr().String()
}

func makeTraceWithStatus(traceIDHex string, status ptrace.StatusCode) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var tid pcommon.TraceID
	copy(tid[:], traceIDHex)
	span.SetTraceID(tid)
	span.Status().SetCode(status)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(100 * time.Millisecond)))
	return td
}

func newTestProcessor(t *testing.T, addr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	f, err := os.CreateTemp("", "proc-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	cfg := &processor.Config{
		BufferDBPath:        f.Name(),
		BufferTTL:           200 * time.Millisecond,
		DropTTL:             500 * time.Millisecond,
		InterestCacheTTL:    5 * time.Second,
		CoordinatorEndpoint: addr,
		Rules: []evaluator.RuleConfig{
			{Type: "error_status"},
		},
	}
	p, err := processor.NewFactory().CreateTracesProcessor(
		context.Background(),
		processortest.NewNopSettings(),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, p.Start(context.Background(), nil))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func TestInterestingTraceIngestedImmediately(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	errTrace := makeTraceWithStatus("trace-error-111111111111111", ptrace.StatusCodeError)
	require.NoError(t, p.ConsumeTraces(context.Background(), errTrace))

	// Wait for buffer_ttl + processing
	time.Sleep(400 * time.Millisecond)

	assert.Equal(t, 1, sink.SpanCount(), "error trace should be ingested")

	// Coordinator should be notified
	time.Sleep(100 * time.Millisecond)
	fc.mu.Lock()
	notified := fc.notified
	fc.mu.Unlock()
	assert.NotEmpty(t, notified, "coordinator should be notified of interesting trace")
}

func TestNonInterestingTraceDropped(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	okTrace := makeTraceWithStatus("trace-ok-11111111111111111", ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))

	// Wait for buffer_ttl + drop_ttl
	time.Sleep(800 * time.Millisecond)

	assert.Equal(t, 0, sink.SpanCount(), "non-interesting trace should be dropped")
}

func TestCoordinatorPushCausesIngestion(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	okTrace := makeTraceWithStatus("trace-ok-22222222222222222", ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))

	// Wait for buffer_ttl evaluation (no ingest yet)
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 0, sink.SpanCount())

	// Coordinator signals: keep this trace
	fc.sendDecision("trace-ok-22222222222222222", true)
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1, sink.SpanCount(), "coordinator push should trigger ingestion")
}
```

Add `"os"` import to `processor_test.go`.

- [ ] **Step 3: Run test to verify it fails**

```bash
go test ./processor/ -v -run TestInteresting
```

Expected: FAIL — `processor.go` does not exist.

- [ ] **Step 4: Implement the processor**

Create `processor/processor.go`:

```go
package processor

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"retroactivesampling/processor/buffer"
	"retroactivesampling/processor/cache"
	coord "retroactivesampling/processor/coordinator"
	"retroactivesampling/processor/evaluator"
)

type retroactiveProcessor struct {
	logger *zap.Logger
	next   consumer.Traces
	buf    *buffer.SpanBuffer
	ic     *cache.InterestCache
	eval   evaluator.Evaluator
	coord  *coord.Client
	cfg    *Config

	mu     sync.Mutex
	timers map[string]*time.Timer // buffer_ttl timers
	drops  map[string]*time.Timer // drop_ttl timers
}

func newProcessor(logger *zap.Logger, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	buf, err := buffer.New(cfg.BufferDBPath)
	if err != nil {
		return nil, err
	}
	chain, err := evaluator.Build(cfg.Rules)
	if err != nil {
		buf.Close()
		return nil, err
	}
	p := &retroactiveProcessor{
		logger: logger,
		next:   next,
		buf:    buf,
		ic:     cache.New(cfg.InterestCacheTTL),
		eval:   chain,
		cfg:    cfg,
		timers: make(map[string]*time.Timer),
		drops:  make(map[string]*time.Timer),
	}
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision)
	return p, nil
}

func (p *retroactiveProcessor) Start(_ context.Context, _ component.Host) error { return nil }

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	p.coord.Close()
	return p.buf.Close()
}

func (p *retroactiveProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (p *retroactiveProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	for traceID, spans := range groupByTrace(td) {
		if p.ic.Has(traceID) {
			if err := p.next.ConsumeTraces(ctx, spans); err != nil {
				return err
			}
			continue
		}
		if err := p.buf.WriteWithEviction(traceID, spans, time.Now()); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", traceID), zap.Error(err))
			if err2 := p.next.ConsumeTraces(ctx, spans); err2 != nil {
				return err2
			}
			continue
		}
		p.resetBufferTimer(traceID)
	}
	return nil
}

func (p *retroactiveProcessor) resetBufferTimer(traceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.timers[traceID]; ok {
		t.Reset(p.cfg.BufferTTL)
		return
	}
	id := traceID
	p.timers[id] = time.AfterFunc(p.cfg.BufferTTL, func() { p.onBufferTimeout(id) })
}

func (p *retroactiveProcessor) onBufferTimeout(traceID string) {
	p.mu.Lock()
	delete(p.timers, traceID)
	p.mu.Unlock()

	traces, ok, err := p.buf.Read(traceID)
	if err != nil || !ok {
		return
	}
	if p.eval.Evaluate(traces) {
		_ = p.next.ConsumeTraces(context.Background(), traces)
		_ = p.buf.Delete(traceID)
		p.ic.Add(traceID)
		p.coord.Notify(traceID)
		return
	}
	// Not interesting locally — start drop timer and wait for coordinator
	p.mu.Lock()
	id := traceID
	p.drops[id] = time.AfterFunc(p.cfg.DropTTL, func() { p.onDropTimeout(id) })
	p.mu.Unlock()
}

func (p *retroactiveProcessor) onDropTimeout(traceID string) {
	p.mu.Lock()
	delete(p.drops, traceID)
	p.mu.Unlock()
	_ = p.buf.Delete(traceID)
}

func (p *retroactiveProcessor) onDecision(traceID string, keep bool) {
	p.mu.Lock()
	if t, ok := p.drops[traceID]; ok {
		t.Stop()
		delete(p.drops, traceID)
	}
	p.mu.Unlock()

	p.ic.Add(traceID)
	if !keep {
		_ = p.buf.Delete(traceID)
		return
	}
	traces, ok, err := p.buf.Read(traceID)
	if err != nil || !ok {
		return
	}
	_ = p.next.ConsumeTraces(context.Background(), traces)
	_ = p.buf.Delete(traceID)
}
```

- [ ] **Step 5: Run all processor tests**

```bash
go test ./processor/... -v -timeout 30s
```

Expected: all tests pass (allow up to 30s for timing-based tests).

- [ ] **Step 6: Commit**

```bash
git add processor/split.go processor/processor.go processor/processor_test.go
git commit -m "feat: processor ConsumeTraces with buffer/evaluate/ingest flow"
```

---

## Phase 5 — End-to-End Integration Test

### Task 11: Full system integration test

**Files:**
- Create: `integration_test.go` (package `integration_test`, at repo root)

- [ ] **Step 1: Write the failing test**

Create `integration_test.go`:

```go
//go:build integration

package integration_test

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
	"google.golang.org/grpc"
	otelprocessor "go.opentelemetry.io/collector/processor"

	gen "retroactivesampling/gen"
	rds "retroactivesampling/coordinator/redis"
	coordserver "retroactivesampling/coordinator/server"
	proc "retroactivesampling/processor"
	"retroactivesampling/processor/evaluator"
)

func startRedis(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "6379")
	return host + ":" + port.Port()
}

func startCoordinator(t *testing.T, redisAddr string) string {
	t.Helper()
	ps := rds.New(redisAddr, 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = ps.Close() })

	srv := coordserver.New(func(traceID string) {
		ok, err := ps.Publish(ctx, traceID)
		if err != nil || !ok {
			return
		}
	})
	go func() {
		_ = ps.Subscribe(ctx, func(traceID string) {
			srv.Broadcast(traceID, true)
		})
	}()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func newTestProcessor(t *testing.T, coordAddr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	f, err := os.CreateTemp("", "e2e-proc-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	cfg := &proc.Config{
		BufferDBPath:        f.Name(),
		BufferTTL:           200 * time.Millisecond,
		DropTTL:             2 * time.Second,
		InterestCacheTTL:    10 * time.Second,
		CoordinatorEndpoint: coordAddr,
		Rules:               []evaluator.RuleConfig{{Type: "error_status"}},
	}
	p, err := proc.NewFactory().CreateTracesProcessor(
		context.Background(),
		processortest.NewNopSettings(),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, p.Start(context.Background(), nil))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func spanWithStatus(traceIDPrefix string, status ptrace.StatusCode) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var tid pcommon.TraceID
	copy(tid[:], traceIDPrefix+"______________")
	span.SetTraceID(tid)
	span.Status().SetCode(status)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(50 * time.Millisecond)))
	return td
}

// TestE2E_ErrorPropagatesAcrossTwoProcessors: Processor A sees error span → notifies coordinator →
// coordinator broadcasts → Processor B (holding non-interesting spans of same trace) ingests them.
func TestE2E_ErrorPropagatesAcrossTwoProcessors(t *testing.T) {
	redisAddr := startRedis(t)
	coordAddr := startCoordinator(t, redisAddr)

	sinkA := &consumertest.TracesSink{}
	sinkB := &consumertest.TracesSink{}
	procA := newTestProcessor(t, coordAddr, sinkA)
	procB := newTestProcessor(t, coordAddr, sinkB)

	traceID := "shared-trace-e2e-123"
	// Processor B buffers non-interesting span for the trace
	require.NoError(t, procB.ConsumeTraces(context.Background(), spanWithStatus(traceID, ptrace.StatusCodeOk)))

	time.Sleep(300 * time.Millisecond) // B evaluates: not interesting, holds in buffer

	// Processor A sees error span for same trace
	require.NoError(t, procA.ConsumeTraces(context.Background(), spanWithStatus(traceID, ptrace.StatusCodeError)))

	time.Sleep(500 * time.Millisecond) // A evaluates → interesting → notifies coordinator → B ingests

	assert.Equal(t, 1, sinkA.SpanCount(), "processor A should ingest its error span")
	assert.Equal(t, 1, sinkB.SpanCount(), "processor B should ingest its buffered span after coordinator push")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -tags integration ./... -v -run TestE2E -timeout 60s
```

Expected: FAIL — build errors (package `coord_main` reference is wrong; fix import to use `server` and `redis` packages directly as shown above).

- [ ] **Step 3: Fix build and run**

Ensure all imports compile: `go build -tags integration ./...`.

```bash
go test -tags integration ./... -v -run TestE2E -timeout 60s
```

Expected: PASS — both processors observed correct span counts.

- [ ] **Step 4: Commit**

```bash
git add integration_test.go
git commit -m "test: end-to-end integration test with two processors and coordinator"
```

---

## Final Verification

- [ ] **Run all unit tests**

```bash
go test ./... -v -timeout 30s
```

Expected: all pass.

- [ ] **Run integration tests**

```bash
go test -tags integration ./... -v -timeout 120s
```

Expected: all pass (requires Docker for testcontainers).

- [ ] **Build all binaries**

```bash
go build ./coordinator/
go build ./processor/...
```

Expected: both build without errors.
