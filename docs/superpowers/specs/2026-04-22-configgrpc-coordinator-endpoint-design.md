# Coordinator gRPC Config Migration — Design Spec

**Date:** 2026-04-22  
**Status:** Approved

---

## Overview

Replace the flat `coordinator_endpoint string` field in the processor config with `go.opentelemetry.io/collector/config/configgrpc.ClientConfig`. This gives users standard otelcol gRPC options (TLS, auth extensions, keepalive, compression, headers) instead of the current hardcoded insecure connection.

---

## Config Change

`Config.CoordinatorEndpoint string` becomes `Config.CoordinatorGRPC configgrpc.ClientConfig` with mapstructure key `coordinator_grpc`.

```go
type Config struct {
    BufferFile              string                      `mapstructure:"buffer_file"`
    MaxBufferBytes          int64                       `mapstructure:"max_buffer_bytes"`
    MaxInterestCacheEntries int                         `mapstructure:"max_interest_cache_entries"`
    CoordinatorGRPC         configgrpc.ClientConfig     `mapstructure:"coordinator_grpc"`
    Policies                []evaluator.PolicyCfg       `mapstructure:"policies"`
}

func (c *Config) Validate() error {
    return c.CoordinatorGRPC.Validate()
}
```

Example YAML:

```yaml
retroactive_sampling:
  coordinator_grpc:
    endpoint: coordinator:9090
    tls:
      insecure: true
```

`createDefaultConfig` is unchanged except removing the old `CoordinatorEndpoint` field — no default endpoint.

---

## Coordinator Client

`coordinator.New` signature:

```go
// Before
func New(endpoint string, handler DecisionHandler, logger *zap.Logger) *Client

// After
func New(grpcCfg configgrpc.ClientConfig, host component.Host, settings component.TelemetrySettings, handler DecisionHandler, logger *zap.Logger) *Client
```

`Client` struct gains `grpcCfg configgrpc.ClientConfig`, `host component.Host`, `settings component.TelemetrySettings` fields; `endpoint string` is removed.

In `connect()`, the dial call becomes:

```go
conn, err := c.grpcCfg.ToClientConn(c.ctx, c.host, c.settings)
```

The log line referencing the endpoint uses `c.grpcCfg.Endpoint`.

---

## Processor Lifecycle

`component.Host` is only available at `Start(ctx, host)`, not at construction time. The processor is updated accordingly:

**`retroactiveProcessor` struct** gains `grpcCfg configgrpc.ClientConfig` and `telSettings component.TelemetrySettings`; the `coord` field starts nil.

**`newProcessor`** stores `grpcCfg` and `telSettings` but does not create the coordinator client.

**New `Start` method:**

```go
func (p *retroactiveProcessor) Start(_ context.Context, host component.Host) error {
    p.coord = coord.New(p.grpcCfg, host, p.telSettings, p.onDecision, p.logger)
    return nil
}
```

**`Shutdown`** guards against nil coord (in case Start was never called):

```go
func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
    if p.coord != nil {
        p.coord.Close()
    }
    err := p.buf.Close()
    p.telemetry.Shutdown()
    return err
}
```

**`factory.go`** adds `processorhelper.WithStart(p.Start)` to the `processorhelper.NewTraces` call.

---

## Dependencies

Add to `processor/retroactivesampling/go.mod`:

```
go.opentelemetry.io/collector/config/configgrpc v0.150.0
```

Run `go mod tidy` after.

---

## Examples

Any YAML files under `examples/` using `coordinator_endpoint: host:port` are updated to the nested `coordinator_grpc:` form.

---

## Testing

Existing unit and integration tests updated to construct `coord.New` with `configgrpc.ClientConfig{Endpoint: "..."}` and `componenttest.NewNopHost()` as the host. No new test scenarios required — TLS/auth is tested by the otelcol configgrpc package itself.
