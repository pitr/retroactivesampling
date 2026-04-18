# Retroactive Sampling — Design Spec

**Date:** 2026-04-18  
**Status:** Approved

---

## Overview

Two Go components:

1. **`processor/`** — an OpenTelemetry Collector processor that buffers spans on disk and makes keep/drop decisions retroactively (after the trace timeout window).
2. **`coordinator/`** — a standalone Go service that receives "interesting trace" notifications from processors and broadcasts them to all connected processors via Redis pub/sub.

The system enables tail-based sampling across a fleet of collectors without requiring a central aggregator to see all spans of a trace.

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                  otelcol (per host)                 │
│                                                     │
│  recv → [retroactive_sampling processor] → exporter │
│              │                  ▲                   │
│         SpanBuffer         InterestCache             │
│         (BBolt DB)         (in-memory TTL)           │
│              │                  │                   │
│         TraceEvaluator    CoordinatorClient          │
│         (pluggable rules)  (gRPC stream)             │
└──────────────────────────┬──────────────────────────┘
                           │ gRPC (processor initiates)
                     ┌─────▼──────┐
                     │  LB / any  │  (standard round-robin)
                     └─────┬──────┘
          ┌────────────────┼────────────────┐
    ┌─────▼──────┐   ┌─────▼──────┐   ┌─────▼──────┐
    │Coordinator │   │Coordinator │   │Coordinator │
    │  instance  │   │  instance  │   │  instance  │
    └─────┬──────┘   └─────┬──────┘   └─────┬──────┘
          └────────────────┼────────────────┘
                     ┌─────▼──────┐
                     │   Redis    │
                     │  pub/sub   │
                     └────────────┘
```

---

## Span Lifecycle in the Processor

```
Span arrives
     │
     ▼
InterestCache hit? ──yes──▶ ingest immediately (skip buffer)
     │ no
     ▼
Write to BBolt:
  - bucket "traces": key=traceID, value=OTLP proto bytes (append on subsequent spans)
  - bucket "order":  key={uint64_bigendian_first_arrival_ns}:{traceID}, value=nil
  In-memory map[traceID]*time.Timer tracks buffer_ttl; timer reset on each new span.
     │
     ▼
buffer_ttl timer fires (reset on each new span for the trace)
     │
     ▼
TraceEvaluator.Evaluate(spans)
     │
     ├─ interesting ──▶ ingest + NotifyCoordinator(traceID)
     │                  + add traceID to InterestCache
     │
     └─ not interesting ──▶ keep in BBolt
                            wait for CoordinatorClient push
                             │
                             ├─ push arrives ──▶ ingest + delete from BBolt
                             │                   + add traceID to InterestCache
                             └─ drop_ttl expires ──▶ delete from BBolt
```

---

## Coordinator Propagation Flow

```
Processor sends NotifyInteresting(traceID)
     │
     ▼
SET retrosampling:decided:{traceID} EX={decided_key_ttl} NX
     │
     ├─ already set (duplicate) ──▶ no-op
     └─ newly set ──▶ PUBLISH retrosampling:interesting {traceID}
                           │
              (all coordinator instances subscribed)
              ▼
     Each coordinator: broadcast TraceDecision{traceID, keep=true}
     to all connected processors via their gRPC stream
```

---

## gRPC Protocol

```protobuf
service Coordinator {
  // Processor initiates; both sides stream on the same connection.
  rpc Connect(stream ProcessorMessage) returns (stream CoordinatorMessage);
}

message ProcessorMessage {
  oneof payload {
    NotifyInteresting notify = 1;
    // future: RuleAck, Heartbeat
  }
}

message CoordinatorMessage {
  oneof payload {
    TraceDecision decision = 1;
    // future: RuleUpdate (OTTL expression + rule ID)
  }
}

message NotifyInteresting {
  string trace_id = 1;
}

message TraceDecision {
  string trace_id = 1;
  // keep=true → ingest buffered spans
  // keep=false reserved for future explicit drop signals
  bool keep = 2;
}
```

---

## Configuration

### Processor (otelcol YAML)

```yaml
processors:
  retroactive_sampling:
    buffer_db_path: /var/otelcol/retrosampling.db
    buffer_ttl: 10s         # wait after last span before evaluating
    drop_ttl: 30s           # wait for coordinator signal before dropping
    interest_cache_ttl: 60s # how long to remember an interesting trace ID
    coordinator_endpoint: coordinator:9090

    rules:
      - type: error_status
      - type: high_latency
        threshold: 5s
```

### Coordinator

```yaml
grpc_listen: :9090
redis_addr: redis:6379
redis_channel: retrosampling:interesting
decided_key_ttl: 60s  # Redis TTL for dedup keys
```

---

## Repository Layout

```
retroactivesampling/
├── go.mod
├── proto/
│   └── coordinator.proto
│
├── processor/
│   ├── factory.go           # otelcol factory + config struct
│   ├── processor.go         # ConsumeTraces entry point
│   ├── buffer/
│   │   └── buffer.go        # BBolt-backed SpanBuffer
│   ├── evaluator/
│   │   ├── evaluator.go     # Evaluator interface + Chain
│   │   ├── error.go         # error status rule
│   │   ├── latency.go       # high latency rule
│   │   └── registry.go      # builds evaluator list from config
│   ├── cache/
│   │   └── cache.go         # InterestCache (sync.Map + TTL)
│   └── coordinator/
│       └── client.go        # gRPC client, reconnect loop
│
└── coordinator/
    ├── main.go
    ├── server/
    │   └── server.go        # gRPC service impl, processor registry
    ├── redis/
    │   └── pubsub.go        # publish + subscribe loop
    └── gen/
        └── coordinator.pb.go
```

### Key Interface

```go
// evaluator/evaluator.go
type Evaluator interface {
    Evaluate(traces ptrace.Traces) bool
}

// Chain short-circuits: any evaluator returning true = interesting
type Chain []Evaluator
func (c Chain) Evaluate(t ptrace.Traces) bool { ... }
```

---

## Failure Modes

| Failure | Behavior |
|---|---|
| Coordinator unreachable at startup | Processor starts normally; `CoordinatorClient` retries with exponential backoff. Local rules evaluate — processor works standalone. |
| Coordinator connection drops | Client reconnects with backoff. Non-interesting buffered spans age out via `drop_ttl`. |
| Processor crashes | BBolt persists. On restart, existing traces re-evaluated on next TTL tick. `InterestCache` lost — in-flight coordinator pushes may be missed. |
| Redis unreachable | Coordinator logs error, continues accepting connections. Cross-instance propagation pauses until Redis recovers. Local interesting traces still ingested. |
| BBolt disk full | Evict oldest entries in FIFO order: cursor.First() on `order` bucket → delete from both `traces` and `order` buckets in a transaction, repeat until write succeeds. |

---

## Testing Strategy

### Unit tests
- Each `Evaluator` impl in isolation
- `Chain` short-circuit logic
- `InterestCache` TTL expiry
- `SpanBuffer` write/read/delete/expiry against a temp BBolt file

### Integration tests (testcontainers — real Redis)
- Golden path: error span on Processor A → ingested + propagated → Processor B ingests buffered spans
- Drop path: non-interesting span dropped after `drop_ttl`
- Coordinator reconnect: kill coordinator mid-test, verify processor recovers
- Processor restart: verify buffered spans survive via BBolt
- Disk-full eviction: fill buffer to cap, verify FIFO eviction

---

## Future Extensions

- **OTTL rules from coordinator**: `RuleUpdate` message in `CoordinatorMessage`; rules stored in Redis, pushed to processors on connect.
- **`keep=false`**: Coordinator explicitly tells processors to drop a trace (e.g., sampled away centrally).
- **Metrics**: expose `retrosampling_buffered_traces`, `retrosampling_ingested_total`, `retrosampling_dropped_total` as OTel metrics from the processor.
