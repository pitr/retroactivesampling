# coordinator

Standalone service that receives interesting-trace notifications from processors, deduplicates them, and broadcasts keep decisions to all connected processors.

## Modes

### Single-node

Runs without external dependencies. Deduplication is in-memory; state is lost on restart. Suitable for development and small single-instance deployments.

### Distributed

Uses Redis for cross-instance deduplication and fan-out. Multiple coordinator instances can run behind a load balancer — each subscribes to the same Redis pub/sub channel and broadcasts to its own connected processors.

### Upstream

Forwards all notifications to a parent coordinator and relays broadcast decisions back to its own connected processors. No local deduplication — the upstream coordinator handles that.

Use this mode to build a daisy-chain topology: collectors in a cluster connect to a local coordinator, which connects upstream to a central coordinator. The chain can be arbitrarily deep; each level uses the same gRPC protocol. This reduces expensive cross-cluster broadcast traffic from `I × P × M` (flat) to `I × K × M` (daisy-chain), where K is the number of clusters. See [PERFORMANCE.md](../PERFORMANCE.md) for the full analysis.

```
collectors → local coordinator → central coordinator → Redis
collectors ←─────────────────────────────────────────┘
```

## Prerequisites

- **single-node:** none
- **distributed:** Redis
- **upstream:** a running coordinator to connect to (any mode)

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

### Upstream

```yaml
grpc_listen: :9090
decided_key_ttl: 60s      # must exceed your trace window
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  upstream:
    endpoint: central-coordinator:9090
```

### Common fields

| Key | Required | Description |
|---|---|---|
| `grpc_listen` | yes | `host:port` to listen for processor gRPC connections |
| `decided_key_ttl` | yes | How long to remember a trace decision; must exceed your longest expected trace window |
| `metrics_listen` | no | If set, expose Prometheus metrics at this `host:port` |
| `shutdown_timeout` | no | Graceful shutdown timeout (default `10s`) |
| `mode` | yes | Exactly one of `single`, `distributed`, or `upstream` must be set |

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

### `mode.upstream` fields

| Key | Required | Description |
|---|---|---|
| `endpoint` | yes | `host:port` of the upstream coordinator to connect to |

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

Outbound broadcast to processors is the dominant cost and scales with `I × collector_count`. See [PERFORMANCE.md](../PERFORMANCE.md) for full traffic formulas, scaling risks, and worked examples including the daisy-chain topology.

## High Availability

**Distributed mode:** run multiple instances behind any load balancer (round-robin). Each instance subscribes to the same Redis pub/sub channel and broadcasts to its own connected processors.

**Upstream mode:** run multiple local coordinator instances per cluster, all pointing at the same upstream endpoint. Each instance independently maintains its upstream connection and broadcasts to its own connected processors. The upstream coordinator deduplicates across all of them.
