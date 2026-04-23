# Coordinator Single-Node Mode Design

**Date:** 2026-04-23
**Status:** Approved

## Problem

The coordinator currently requires Redis for all deployments. Redis serves two purposes:
1. **Deduplication** — SET NX with `decided_key_ttl` prevents the same trace from being broadcast twice across multiple coordinator instances.
2. **Cross-instance fan-out** — PUBLISH/SUBSCRIBE ensures all coordinator instances see every interesting trace regardless of which instance received it.

In single-node deployments (dev, small setups), requiring Redis is unnecessary overhead.

## Goal

Add a single-node mode where the coordinator runs without Redis, respecting `decided_key_ttl` via an in-memory store. The config structure must be extensible for future modes (e.g. daisy-chained coordinators), each with their own sub-config.

## Architecture

### PubSub interface

A `PubSub` interface is extracted to the root `coordinator` package. Both the existing `redis.PubSub` and the new `memory.PubSub` implement it. `server.go` is unchanged — it receives the interface via `main.go`.

```go
// coordinator/pubsub.go
type PubSub interface {
    Publish(ctx context.Context, traceID string) (bool, error)
    Subscribe(ctx context.Context, handler func(string)) error
    Close() error
}
```

`redis.PubSub` already satisfies this interface — no changes to that file.

### memory package

`coordinator/memory/pubsub.go` implements `PubSub` using `patrickmn/go-cache` for deduplication.

- `Publish`: calls `cache.Add(traceID, struct{}{}, ttl)` — atomic SET NX semantics (returns error if key exists). If new, calls all registered handlers synchronously inline.
- `Subscribe`: appends handler to a slice (guarded by `sync.RWMutex`), then blocks on `ctx.Done()`.
- `Close`: no-op.

Calling handlers synchronously from `Publish` is safe: `onNotify` is invoked from `server.Connect`'s recv loop without holding `s.mu`, and `Broadcast` only acquires `s.mu` independently.

### Config structure

Tagged union — each mode name is the YAML key, its sub-config lives under it. Adding a future mode = add one field to `ModeConfig`.

```go
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

type SingleConfig struct{}

type DistributedConfig struct {
    RedisPrimary  redis.Config   `yaml:"redis_primary"`
    RedisReplicas []redis.Config `yaml:"redis_replicas"`
}
```

`ModeConfig.active()` is a helper that returns `any` (the single non-nil sub-config pointer) or calls `log.Fatal` if zero or two mode fields are set. Caller uses a type switch. Redis config is invisible in any non-distributed mode.

**Single-node YAML:**
```yaml
grpc_listen: :9090
decided_key_ttl: 60s
mode:
  single: {}
```

**Distributed YAML:**
```yaml
grpc_listen: :9090
decided_key_ttl: 60s
mode:
  distributed:
    redis_primary:
      endpoint: redis:6379
    redis_replicas:
      - endpoint: replica1:6379
```

### main.go wiring

```go
switch m := cfg.Mode.active().(type) {
case *SingleConfig:
    ps = memory.New(cfg.DecidedKeyTTL)
case *DistributedConfig:
    // validate m.RedisPrimary.Endpoint, build redis.PubSub
}
```

## Files changed

| File | Change |
|------|--------|
| `coordinator/pubsub.go` | NEW — PubSub interface |
| `coordinator/memory/pubsub.go` | NEW — in-memory implementation |
| `coordinator/config.go` | Add ModeConfig, SingleConfig, DistributedConfig; remove top-level Redis fields |
| `coordinator/main.go` | Use PubSub interface; branch on mode; update validation |
| `coordinator/go.mod` | Add patrickmn/go-cache dependency |
| `coordinator/README.md` | Document modes, update config tables, update prerequisites |

## Dependencies

- `patrickmn/go-cache` — in-memory TTL cache with atomic Add (SET NX semantics). No new dep for distributed mode.

## Testing

- `memory/pubsub_test.go` — unit tests: dedup (same trace not broadcast twice), TTL expiry, multi-subscriber, concurrent Publish safety.
- Existing `server/server_test.go` and `redis/pubsub_test.go` unchanged.
- `main_test.go` or config test: validate that mixed mode fields (both single and distributed set) fatal correctly.
