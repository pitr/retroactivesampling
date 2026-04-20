# Retroactive Sampling

Tail-based sampling for OpenTelemetry without a central aggregator.

Spans are buffered on disk per collector host. A trace is kept if any collector sees it as interesting (error status, high latency). Coordinators propagate that decision to all other collectors via Redis pub/sub.

Inspired by [KubeCon presentation from VictoriaMetrics folks](https://www.youtube.com/watch?v=ehKzt_kGEeM)

## Components

- **`coordinator/`** — standalone service: receives interesting-trace notifications via gRPC, deduplicates with Redis `SET NX`, broadcasts decisions to all connected processors.
- **`processor/`** — otelcol processor plugin.
- **`proto/`** — protobuf definitions for gRPC communication between coordinator and processor.
- **`example/`** — config examples for coordinator and collector.
- **`cmd/`** — various commands.

## Running the coordinator

### Build

```bash
make build
# produces bin/coordinator
```

### Configure

Edit `example/coordinator.yaml`:

```yaml
grpc_listen: :9090        # gRPC listen address for processor connections
redis_addr: redis:6379    # Redis for cross-instance coordination
decided_key_ttl: 60s      # dedup key TTL — must exceed your trace lifetime
```

### Run

```bash
bin/coordinator --config example/coordinator.yaml
```

For HA, run multiple instances behind any load balancer (standard round-robin). Each instance subscribes to the same Redis channel and broadcasts to its own connected processors.

## Building the collector

Install the [OpenTelemetry Collector Builder](https://github.com/open-telemetry/opentelemetry-collector/tree/main/cmd/builder):

```bash
go install go.opentelemetry.io/collector/cmd/builder@v0.150.0
```

Build a collector binary with the retroactive_sampling processor included:

```bash
make collector
# produces bin/otelcol-retrosampling
```

## Running the collector

```bash
bin/otelcol-retrosampling --config example/otelcol.yaml
```

Processor config options:

| Key | Default | Description |
|---|---|---|
| `buffer_dir` | — | Directory for buffered trace files |
| `max_buffer_bytes` | `0` (unlimited) | Max bytes of buffer disk usage; oldest traces evicted first |
| `max_interest_cache_entries` | `100000` | Max interesting trace IDs cached for fast-path routing |
| `coordinator_endpoint` | — | `host:port` of coordinator gRPC server |
| `rules` | — | List of sampling rules (see below) |

### Rules

```yaml
rules:
  - type: error_status          # keep any trace with a StatusCodeError span
  - type: high_latency
    threshold: 5s               # keep any trace with a span >= 5s duration
```

Rules are evaluated with OR logic — any match marks the trace as interesting.

## Development

```bash
make test                # unit tests
make test-integration    # integration tests (requires Docker)
make proto               # regenerate gRPC code from proto/coordinator.proto
```
