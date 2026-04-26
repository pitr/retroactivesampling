# PubSub Redesign: Constructor-Injected Handler

## Problem

`PubSub.Subscribe(ctx, handler func([]byte)) error` has three issues:

1. Handler func in the interface is odd — it reads like a registration call but also blocks.
2. `memory` and `proxy` maintain `handlers []func([]byte)` + `mu sync.RWMutex` to support multiple subscribers, which never happens in practice.
3. The lock on a slice that only ever has one element is unnecessary complexity.

## Decision

Move `handler func([]byte)` to each implementation's constructor. Remove `Subscribe` from the `PubSub` interface entirely.

## Interface

```go
type PubSub interface {
    Publish(ctx context.Context, traceID []byte) (bool, error)
    Close() error
}
```

## Per-Implementation Changes

### memory.PubSub

- `New(ttl time.Duration, handler func([]byte)) *PubSub`
- `handler func([]byte)` stored as a single field — no slice, no mutex.
- `Publish` calls handler directly (synchronous, same goroutine as caller).
- `Close` remains a no-op.

### proxy.PubSub

- `New(endpoint string, handler func([]byte)) *PubSub`
- `handler func([]byte)` stored as a single field — no slice, no mutex.
- `recv` goroutine (already started in `New`) calls the stored handler directly.

### redis.PubSub

- `New(writeCfg Config, readCfg *Config, ttl time.Duration, handler func([]byte)) (*PubSub, error)`
- `handler func([]byte)` stored as a single field.
- Add `ctx context.Context`, `cancel context.CancelFunc`, `done chan struct{}` fields (same pattern as proxy).
- `New` creates the internal context and starts a receive goroutine.
- `Close` cancels the internal context, waits for the goroutine (`<-done`), then closes the clients.

## main.go

Forward-declare `srv` to break the circular dependency (ps needs `srv.Broadcast`; srv needs `ps.Publish`):

```go
var srv *server.Server
// ... mode switch: pass handler closure to each New ...
ps = memory.New(m.DecidedKeyTTL, func(id []byte) { srv.Broadcast(id) })

srv = server.New(func(traceID []byte) {
    if novel, err := ps.Publish(ctx, traceID); ...
}, ...)
```

Safe: the handler is not called until a processor sends a `NotifyInteresting`, which requires a connection to the gRPC server — started after `srv` is assigned.

Remove the `go func() { ps.Subscribe(...) }()` block entirely.

## Tests

- `memory/pubsub_test.go`: Remove `TestPublishCallsAllSubscribers` (multi-subscriber is no longer supported). Update all tests to pass handler to `New`.
- `proxy/pubsub_test.go`: Pass handler to `New`; remove `Subscribe` calls.
- `proxy/integration_test.go`: Same.
- `redis/pubsub_test.go`: Pass handler to `New`; remove `Subscribe` calls.

## What This Does Not Change

- Wire protocol, config, gRPC server, metrics — untouched.
- `Publish` semantics (dedup, novel bool, error) — unchanged.
- `Close` semantics — unchanged for memory/proxy; redis gains wait-for-goroutine behaviour it lacked before.
