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
grpc_listen: :9090
decided_key_ttl: 60s      # must exceed your trace window
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

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

| Key | Required | Description |
|---|---|---|
| `grpc_listen` | yes | `host:port` to listen for processor gRPC connections |
| `decided_key_ttl` | yes | How long to remember a trace decision; must exceed your longest expected trace window |
| `metrics_listen` | no | If set, expose Prometheus metrics at this `host:port` |
| `shutdown_timeout` | no | Graceful shutdown timeout (default `10s`) |
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

Run multiple instances behind any load balancer (round-robin). Each instance subscribes to the same Redis pub/sub channel and broadcasts to its own connected processors.
