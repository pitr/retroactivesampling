# coordinator

Standalone service that receives interesting-trace notifications from processors, deduplicates with Redis, and broadcasts keep decisions to all connected processors.

## Prerequisites

- Redis

## Build

```bash
make build
# produces bin/coordinator
```

## Configuration

```yaml
grpc_listen: :9090        # gRPC listen address for processor connections
redis_addr: redis:6379    # Redis address
decided_key_ttl: 60s      # dedup key TTL — must exceed your trace window
metrics_listen: :9091     # Prometheus metrics endpoint (optional)
```

| Key | Required | Description |
|---|---|---|
| `grpc_listen` | yes | `host:port` to listen for processor gRPC connections |
| `redis_addr` | yes | Redis `host:port` |
| `decided_key_ttl` | yes | How long to remember a trace decision; must exceed your longest expected trace window |
| `metrics_listen` | no | If set, expose Prometheus metrics at this `host:port` |

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

Run multiple instances behind any load balancer (round-robin). Each instance subscribes to the same Redis pub/sub channel and broadcasts to its own connected processors.
