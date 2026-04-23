# Daisy-Chain Coordinators Design

**Date:** 2026-04-23  
**Status:** Approved

## Problem

Fleet-total coordinator→processor broadcast traffic scales as `I × P × M`. With P=10k collectors and I=10k interesting traces/s, that is 2 GB/s fleet-wide. Adding coordinator instances reduces per-instance load but not fleet total. Cross-cluster broadcast is the expensive part when collectors span multiple geographic clusters.

## Goal

Reduce cross-cluster broadcast bandwidth by allowing coordinators to be chained: collectors in a cluster connect to a local coordinator, which connects to a central (or higher-level) coordinator. The chain can be arbitrarily deep. Each coordinator is agnostic to what is upstream or downstream — the gRPC protocol is identical at every level.

## Approach

Add a third `PubSub` implementation — `upstream` — that wraps a gRPC coordinator client. A coordinator running in `upstream` mode acts as both a server (accepting downstream connections from collectors or other coordinators) and a client (forwarding notifies to and receiving decisions from an upstream coordinator).

No local deduplication. Every `NotifyInteresting` received from downstream is forwarded upstream. The upstream coordinator (or Redis at the top) handles deduplication. Duplicate notifies from multiple local collectors seeing the same trace are harmless — upstream deduplication absorbs them, and notify traffic is already low-risk.

## Architecture

```
coordinator/
  upstream/
    pubsub.go    ← new PubSub implementation
```

Three `PubSub` implementations:

| Implementation | Dedup | Broadcast mechanism |
|----------------|-------|---------------------|
| `memory`       | local in-memory (go-cache) | local in-process |
| `redis`        | Redis SET NX | Redis pub/sub |
| `upstream`     | none (delegated upstream) | upstream coordinator gRPC |

## Config

New third mode in `ModeConfig`:

```yaml
mode:
  upstream:
    endpoint: "central-coordinator:4317"
    # tls, headers, etc. — standard gRPC client fields
```

`UpstreamConfig` holds a gRPC endpoint (and optionally TLS/auth). Uses plain `google.golang.org/grpc` — no OTel `configgrpc` dependency, consistent with the rest of the coordinator binary.

## Data Flow

**2-level example:**

```
collector ──NotifyInteresting──► local-coordinator ──NotifyInteresting──► central-coordinator ──► Redis
                                                                                  │
collector ◄──BatchTraceDecision── local-coordinator ◄──BatchTraceDecision────────┘
```

**Notify path:**
1. Collector sends `NotifyInteresting` to local coordinator
2. `server.Connect` receive loop calls `onNotify` → `ps.Publish`
3. `upstream.PubSub.Publish` enqueues traceID to upstream gRPC stream
4. Central coordinator deduplicates (Redis SET NX) and broadcasts to all connected local coordinators
5. `upstream.PubSub.Subscribe` handler fires → `srv.Broadcast` → all local collector streams

**main.go wiring is unchanged** — `onNotify` → `ps.Publish`, `ps.Subscribe(ctx, srv.Broadcast)` works identically for all three modes.

## `upstream.PubSub` Behavior

- `Publish(ctx, traceID)` — enqueues `NotifyInteresting` to buffered send channel; always returns `(true, nil)` (no local dedup)
- `Subscribe(ctx, handler)` — reads `BatchTraceDecision` messages from upstream stream; calls handler per traceID
- Reconnects with exponential backoff (1s → 30s cap) on stream error or EOF, same pattern as processor coordinator client
- During reconnect: send channel full → notifies dropped (best-effort, same as processor client)

## Performance Impact

Parameters: P=10,000 collectors, I=10,000 interesting traces/s, M=20 B, 100 clusters of 100 collectors each, 1 local coordinator per cluster.

| Path | Flat topology | Daisy-chain |
|------|--------------|-------------|
| Cross-cluster broadcast (central → all) | `I × P × M` = 2 GB/s | `I × C_local × M` = 2 MB/s |
| Per-local-coordinator → its collectors | — | `I × (P/C_local) × M` = 2 MB/s |
| Fleet-total broadcast | 2 GB/s | 2 GB/s (unchanged) |
| Local → central notifies | `I × M` = 200 KB/s | up to `I × M × S` worst-case |

Cross-cluster broadcast: **1000× reduction** (P/C_local = 100) for this example. Fleet-total bytes are unchanged — the fan-out is two-stage rather than one-stage. The expensive cross-cluster hop shrinks from P messages per decision to C_local messages per decision.

## Error Handling

- Stream disconnect → reconnect with backoff; no degraded local-only mode (out of scope)
- Send queue full during reconnect → notifies dropped; upstream dedup means this is best-effort
- Context cancellation → `Subscribe` unblocks, `Publish` returns error

## Testing

- **Unit:** mock upstream gRPC server; verify `Publish` sends `NotifyInteresting`; verify `Subscribe` handler fires on `BatchTraceDecision`; verify reconnect on stream error
- **Integration:** wire two coordinators (central=single-node, local=upstream); send `NotifyInteresting` via local; assert `BatchTraceDecision` reaches local collector stream
