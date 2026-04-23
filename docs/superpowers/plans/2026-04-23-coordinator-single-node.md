# Coordinator Single-Node Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a single-node coordinator mode that runs without Redis, using an in-memory TTL cache for deduplication.

**Architecture:** Extract a `PubSub` interface (package main) satisfied by both `redis.PubSub` (unchanged) and a new `memory.PubSub`. Config gains a tagged-union `mode:` block so each mode (single, distributed, future) owns its sub-config exclusively. `main.go` branches on the active mode to wire the correct implementation.

**Tech Stack:** Go, `github.com/patrickmn/go-cache` v2.1.0+incompatible (in-memory TTL cache), existing `testify` for tests.

---

## File Map

| File | Action | Responsibility |
|------|--------|---------------|
| `coordinator/pubsub.go` | Create | `PubSub` interface definition + compile-time checks |
| `coordinator/memory/pubsub.go` | Create | In-memory PubSub implementation using go-cache |
| `coordinator/memory/pubsub_test.go` | Create | Unit tests for memory.PubSub |
| `coordinator/config.go` | Modify | Replace top-level Redis fields with `ModeConfig` tagged union |
| `coordinator/main.go` | Modify | Use `PubSub` interface; branch on mode; update validation |
| `coordinator/go.mod` | Modify | Add `github.com/patrickmn/go-cache` dependency |
| `coordinator/README.md` | Modify | Document modes, update config tables, update prerequisites |

---

## Task 1: Add go-cache dependency

**Files:**
- Modify: `coordinator/go.mod`
- Modify: `coordinator/go.sum`

- [ ] **Step 1: Add the dependency**

Run from `coordinator/` directory:
```bash
go get github.com/patrickmn/go-cache@v2.1.0+incompatible
```

Expected output: line added to `go.mod` and `go.sum` updated.

- [ ] **Step 2: Tidy**

```bash
go mod tidy
```

Expected: no errors, `go.sum` consistent.

- [ ] **Step 3: Verify build still passes**

```bash
go build ./...
```

Expected: PASS (no new code yet, just dep added).

- [ ] **Step 4: Commit**

```bash
git add coordinator/go.mod coordinator/go.sum
git commit -m "chore(coordinator): add go-cache dependency"
```

---

## Task 2: Extract PubSub interface

**Files:**
- Create: `coordinator/pubsub.go`

- [ ] **Step 1: Create the interface file**

Create `coordinator/pubsub.go`:

```go
package main

import "context"

type PubSub interface {
	Publish(ctx context.Context, traceID string) (bool, error)
	Subscribe(ctx context.Context, handler func(string)) error
	Close() error
}
```

- [ ] **Step 2: Build to confirm `redis.PubSub` already satisfies it**

```bash
go build ./...
```

Expected: PASS. `redis.PubSub` has `Publish(context.Context, string) (bool, error)`, `Subscribe(context.Context, func(string)) error`, and `Close() error` — all matching.

- [ ] **Step 3: Commit**

```bash
git add coordinator/pubsub.go
git commit -m "feat(coordinator): extract PubSub interface"
```

---

## Task 3: Implement memory.PubSub (TDD)

**Files:**
- Create: `coordinator/memory/pubsub.go`
- Create: `coordinator/memory/pubsub_test.go`

- [ ] **Step 1: Write the failing tests**

Create `coordinator/memory/pubsub_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
go test ./memory/...
```

Expected: FAIL — `memory` package does not exist yet.

- [ ] **Step 3: Implement memory.PubSub**

Create `coordinator/memory/pubsub.go`:

```go
package memory

import (
	"context"
	"sync"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

type PubSub struct {
	cache    *gocache.Cache
	mu       sync.RWMutex
	handlers []func(string)
}

func New(ttl time.Duration) *PubSub {
	return &PubSub{
		cache: gocache.New(ttl, ttl*2),
	}
}

func (p *PubSub) Publish(_ context.Context, traceID string) (bool, error) {
	if p.cache.Add(traceID, struct{}{}, gocache.DefaultExpiration) != nil {
		return false, nil
	}
	p.mu.RLock()
	hs := p.handlers
	p.mu.RUnlock()
	for _, h := range hs {
		h(traceID)
	}
	return true, nil
}

func (p *PubSub) Subscribe(ctx context.Context, handler func(string)) error {
	p.mu.Lock()
	p.handlers = append(p.handlers, handler)
	p.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (p *PubSub) Close() error { return nil }
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
go test -race ./memory/...
```

Expected: PASS, including race detector (concurrent publish test).

- [ ] **Step 5: Add compile-time interface check to `coordinator/pubsub.go`**

Edit `coordinator/pubsub.go` to add checks after the interface declaration:

```go
package main

import (
	"context"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/redis"
)

type PubSub interface {
	Publish(ctx context.Context, traceID string) (bool, error)
	Subscribe(ctx context.Context, handler func(string)) error
	Close() error
}

var (
	_ PubSub = (*redis.PubSub)(nil)
	_ PubSub = (*memory.PubSub)(nil)
)
```

- [ ] **Step 6: Build to confirm both types satisfy the interface**

```bash
go build ./...
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add coordinator/memory/ coordinator/pubsub.go
git commit -m "feat(coordinator): add in-memory PubSub implementation"
```

---

## Task 4: Refactor config and wire main.go

**Files:**
- Modify: `coordinator/config.go`
- Modify: `coordinator/main.go`

This task changes the config schema (breaking) and updates `main.go` in one commit since `config.go` changes break the build until `main.go` is updated.

- [ ] **Step 1: Replace `coordinator/config.go`**

Write the new `coordinator/config.go` in full:

```go
package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"pitr.ca/retroactivesampling/coordinator/redis"
)

type Config struct {
	GRPCListen      string        `yaml:"grpc_listen"`
	DecidedKeyTTL   time.Duration `yaml:"decided_key_ttl"`
	MetricsListen   string        `yaml:"metrics_listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	Mode            ModeConfig    `yaml:"mode"`
}

type ModeConfig struct {
	Single      *SingleConfig      `yaml:"single"`
	Distributed *DistributedConfig `yaml:"distributed"`
}

func (m ModeConfig) active() (any, error) {
	var results []any
	if m.Single != nil {
		results = append(results, m.Single)
	}
	if m.Distributed != nil {
		results = append(results, m.Distributed)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("exactly one mode must be configured (got %d)", len(results))
	}
	return results[0], nil
}

type SingleConfig struct{}

type DistributedConfig struct {
	RedisPrimary  redis.Config   `yaml:"redis_primary"`
	RedisReplicas []redis.Config `yaml:"redis_replicas"`
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

- [ ] **Step 2: Update `coordinator/main.go`**

Write the new `coordinator/main.go` in full:

```go
package main

import (
	"context"
	"flag"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/redis"
	"pitr.ca/retroactivesampling/coordinator/server"
	gen "pitr.ca/retroactivesampling/proto"
)

func main() {
	cfgPath := flag.String("config", "coordinator.yaml", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if cfg.GRPCListen == "" {
		log.Fatal("grpc_listen is required")
	}
	if cfg.DecidedKeyTTL == 0 {
		log.Fatal("decided_key_ttl is required")
	}

	activeMode, err := cfg.Mode.active()
	if err != nil {
		log.Fatalf("config mode: %v", err)
	}

	bytesIn := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_grpc_bytes_received_total",
		Help: "Total bytes received from processors via gRPC.",
	})
	bytesOut := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_grpc_bytes_sent_total",
		Help: "Total bytes sent to processors via gRPC.",
	})
	interestingTraces := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_interesting_traces_total",
		Help: "Total unique interesting traces seen by this coordinator.",
	})
	droppedSends := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_broadcast_drops_total",
		Help: "Total keep decisions dropped because a processor stream's send buffer was full.",
	})
	sendErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_broadcast_send_errors_total",
		Help: "Total errors returned by stream.Send when broadcasting keep decisions.",
	})
	prometheus.MustRegister(bytesIn, bytesOut, interestingTraces, droppedSends, sendErrors)

	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		go func() {
			log.Printf("metrics listening on %s", cfg.MetricsListen)
			if err := http.ListenAndServe(cfg.MetricsListen, mux); err != nil {
				log.Printf("metrics server: %v", err)
			}
		}()
	}

	var ps PubSub
	switch m := activeMode.(type) {
	case *SingleConfig:
		log.Printf("running in single-node mode")
		ps = memory.New(cfg.DecidedKeyTTL)
	case *DistributedConfig:
		if m.RedisPrimary.Endpoint == "" {
			log.Fatal("distributed mode: redis_primary.endpoint is required")
		}
		if len(m.RedisReplicas) > 0 {
			replicaCfg := m.RedisReplicas[rand.Intn(len(m.RedisReplicas))]
			log.Printf("subscribing to Redis replica %s", replicaCfg.Endpoint)
			ps, err = redis.NewWithReplica(m.RedisPrimary, replicaCfg, cfg.DecidedKeyTTL)
		} else {
			ps, err = redis.New(m.RedisPrimary, cfg.DecidedKeyTTL)
		}
		if err != nil {
			log.Fatalf("redis: %v", err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := server.New(func(traceID string) {
		if ctx.Err() != nil {
			return
		}
		if novel, err := ps.Publish(ctx, traceID); err != nil {
			log.Printf("publish %s: %v", traceID, err)
		} else if novel {
			interestingTraces.Add(1)
		}
	}, bytesIn, bytesOut, droppedSends, sendErrors)

	go func() {
		if err := ps.Subscribe(ctx, func(traceID string) {
			srv.Broadcast(traceID)
		}); err != nil {
			log.Printf("subscribe error: %v", err)
		}
	}()

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)

	go func() {
		<-ctx.Done()
		log.Printf("shutting down")
		timeout := cfg.ShutdownTimeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), timeout)
		defer stopCancel()
		done := make(chan struct{})
		go func() {
			gs.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-stopCtx.Done():
			log.Printf("graceful stop timed out, forcing")
			gs.Stop()
		}
		_ = ps.Close()
		log.Printf("shutdown complete")
	}()

	log.Printf("coordinator listening on %s", cfg.GRPCListen)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
```

- [ ] **Step 3: Build**

```bash
go build ./...
```

Expected: PASS.

- [ ] **Step 4: Run all tests**

```bash
go test -race ./...
```

Expected: PASS (integration tests skipped unless `-tags integration` is passed).

- [ ] **Step 5: Commit**

```bash
git add coordinator/config.go coordinator/main.go
git commit -m "feat(coordinator): add single-node mode with in-memory PubSub"
```

---

## Task 5: Update README

**Files:**
- Modify: `coordinator/README.md`

- [ ] **Step 1: Rewrite `coordinator/README.md`**

Replace the entire file with:

```markdown
# coordinator

Standalone service that receives interesting-trace notifications from processors, deduplicates them, and broadcasts keep decisions to all connected processors.

## Modes

### Single-node

Runs without external dependencies. Deduplication is in-memory; state is lost on restart. Suitable for development and small single-instance deployments.

### Distributed

Uses Redis for deduplication and cross-instance fan-out. Multiple coordinator instances can run behind a load balancer — each subscribes to the same Redis pub/sub channel and broadcasts to its own connected processors.

## Prerequisites

- **single-node:** none
- **distributed:** Redis

## Build

```bash
make build
# produces bin/coordinator
```

## Configuration

### Single-node

```yaml
grpc_listen: :9090
decided_key_ttl: 60s      # must exceed your trace window
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  single: {}
```

### Distributed

```yaml
grpc_listen: :9090
decided_key_ttl: 60s      # must exceed your trace window
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  distributed:
    redis_primary:
      endpoint: redis:6379
      username: user           # optional
      password: secret         # optional
      tls:
        enabled: true
        ca_file: /etc/ssl/ca.crt
        cert_file: /etc/ssl/client.crt
        key_file: /etc/ssl/client.key
    redis_replicas:            # optional; each coordinator picks one at random for SUBSCRIBE
      - endpoint: replica1:6379
      - endpoint: replica2:6379
```

### Common fields

| Key | Required | Description |
|---|---|---|
| `grpc_listen` | yes | `host:port` to listen for processor gRPC connections |
| `decided_key_ttl` | yes | How long to remember a trace decision; must exceed your longest expected trace window |
| `metrics_listen` | no | If set, expose Prometheus metrics at this `host:port` |
| `shutdown_timeout` | no | Graceful shutdown timeout (default `10s`) |
| `mode` | yes | Exactly one of `single` or `distributed` must be set |

### `mode.distributed` fields

| Key | Required | Description |
|---|---|---|
| `redis_primary` | yes | Redis primary connection (used for SET NX + PUBLISH) |
| `redis_replicas` | no | Redis replica connections. Each coordinator picks one at random for SUBSCRIBE, distributing Redis outbound fan-out across replicas. Falls back to primary if not set. |

**`redis_primary` / `redis_replicas[]` fields:**

| Key | Description |
|---|---|
| `endpoint` | `host:port` (or socket path if `transport: unix`) |
| `transport` | `tcp` (default) or `unix` |
| `client_name` | Name sent via `CLIENT SETNAME` on each connection; visible in `CLIENT LIST` for monitoring |
| `username` | ACL username |
| `password` | Password |
| `db` | Database number (default `0`) |
| `max_retries` | Max retries before giving up (`-1` disables, default `3`) |
| `dial_timeout` | Connection timeout (default `5s`) |
| `read_timeout` | Socket read timeout (default `3s`) |
| `write_timeout` | Socket write timeout (default `3s`) |
| `tls.enabled` | Enable TLS |
| `tls.insecure_skip_verify` | Skip server certificate verification |
| `tls.ca_file` | Path to CA certificate PEM |
| `tls.cert_file` | Path to client certificate PEM |
| `tls.key_file` | Path to client key PEM |

## Run

```bash
bin/coordinator --config coordinator.yaml
```

## Performance

The coordinator receives far less traffic than the collector fleet — multiple orders of magnitude less. Collectors notify the coordinator only once per *interesting* trace (not per span), so coordinator inbound scales with the interesting-trace rate `I`, not the raw span rate:

```
fleet inbound:        I × message_size   (~200 KB/s at I=10k, 20 B/message)
per coordinator:      I × message_size / coordinator_count
```

Outbound broadcast to processors is the dominant cost and scales with `I × collector_count`. See [PERFORMANCE.md](../PERFORMANCE.md) for full traffic formulas, scaling risks, and worked examples.

## High Availability

Applies to **distributed mode** only. Run multiple instances behind any load balancer (round-robin). Each instance subscribes to the same Redis pub/sub channel and broadcasts to its own connected processors.
```

- [ ] **Step 2: Build and test to confirm nothing broken**

```bash
go build ./... && go test -race ./...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add coordinator/README.md
git commit -m "docs(coordinator): document single-node and distributed modes"
```

---

## Self-Review

**Spec coverage:**
- [x] Single-node mode without Redis → Task 3 (memory.PubSub) + Task 4 (wiring)
- [x] Respects `decided_key_ttl` in single-node → `memory.New(cfg.DecidedKeyTTL)` in Task 4
- [x] Extensible mode config (tagged union) → `ModeConfig` in Task 4
- [x] Redis config only in distributed mode → `DistributedConfig` nests it, invisible otherwise
- [x] Update coordinator README → Task 5

**Placeholder scan:** No TBDs. All code blocks complete.

**Type consistency:**
- `PubSub` interface defined in Task 2, used as `var ps PubSub` in Task 4 ✓
- `memory.New(ttl)` returns `*memory.PubSub` (satisfies `PubSub`) — checked in Task 3 Step 5 ✓
- `ModeConfig.active()` returns `(any, error)` — type switch in Task 4 uses `*SingleConfig` / `*DistributedConfig` matching Task 4 Step 1 definitions ✓
- `gocache.DefaultExpiration` works because `New` passes `ttl` as the default expiration to `gocache.New` ✓
