# coordinator

Standalone service that receives interesting-trace notifications from processors, deduplicates them, and broadcasts keep decisions to all connected processors.

## Modes

### Single-node

Runs without external dependencies. Deduplication is in-memory; state is lost on restart. Suitable for development and small single-instance deployments.

### Distributed

Uses Redis for cross-instance deduplication and fan-out. Multiple coordinator instances can run behind a load balancer — each subscribes to the same Redis pub/sub channel and broadcasts to its own connected processors.

### Proxy

Forwards all notifications to a parent coordinator and relays broadcast decisions back to its own connected processors. No local deduplication — the parent coordinator handles that.

Use this mode to build a daisy-chain topology: collectors in a cluster connect to a local coordinator, which connects to a central coordinator. The chain can be arbitrarily deep; each level uses the same gRPC protocol. This reduces expensive cross-cluster broadcast traffic from `I × P × M` (flat) to `I × K × M` (daisy-chain), where K is the number of clusters. See [PERFORMANCE.md](../PERFORMANCE.md) for the full analysis.

```
collectors → local coordinator → central coordinator → Redis
collectors ←─────────────────────────────────────────┘
```

## Prerequisites

- **single-node:** none
- **distributed:** Redis
- **proxy:** a running coordinator to connect to (any mode)

## Build

```bash
make build
# produces bin/coordinator
```

## Configuration

### Single-node

```yaml
grpc_listen: :9090
log_level: INFO           # optional, default INFO
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  single:
    decided_key_ttl: 60s  # must exceed your trace window
```

### Distributed

```yaml
grpc_listen: :9090
log_level: INFO           # optional, default INFO
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  distributed:
    decided_key_ttl: 60s  # must exceed your trace window
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

### Proxy

```yaml
grpc_listen: :9090
log_level: INFO           # optional, default INFO
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  proxy:
    endpoint: central-coordinator:9090
    tls:                                   # optional; insecure if omitted
      ca_file: /etc/ssl/ca.crt
      cert_file: /etc/ssl/client.crt       # optional; required for mTLS
      key_file: /etc/ssl/client.key        # required if cert_file set
    headers:                               # optional
      authorization: "Bearer xyz"
      x-tenant-id: "abc"
    compression: gzip                      # optional; "" or "gzip"
    keepalive:                             # optional
      time: 30s
      timeout: 10s
      permit_without_stream: true
```

### Common fields

| Key | Required | Description |
|---|---|---|
| `grpc_listen` | yes | `host:port` to listen for processor gRPC connections |
| `log_level` | no | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR` (default `INFO`) |
| `metrics_listen` | no | If set, expose Prometheus metrics at this `host:port` |
| `shutdown_timeout` | no | Graceful shutdown timeout (default `10s`) |
| `tls` | no | If set, serve gRPC over TLS. See [Server TLS fields](#server-tls-fields) |
| `mode` | yes | Exactly one of `single`, `distributed`, or `proxy` must be set |

### Server TLS fields

| Key | Description |
|---|---|
| `cert_file` | Path to server certificate PEM (required) |
| `key_file` | Path to server private key PEM (required) |
| `client_ca_file` | Optional. If set, require client certificates signed by this CA (mTLS) |

Example:

```yaml
tls:
  cert_file: /etc/ssl/server.crt
  key_file: /etc/ssl/server.key
  client_ca_file: /etc/ssl/ca.crt   # optional; enables mTLS
```

### `mode.single` fields

| Key | Required | Description |
|---|---|---|
| `decided_key_ttl` | yes | How long to remember a trace decision; must exceed your longest expected trace window |

### `mode.distributed` fields

| Key | Required | Description |
|---|---|---|
| `decided_key_ttl` | yes | How long to remember a trace decision; must exceed your longest expected trace window |
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

### `mode.proxy` fields

| Key | Required | Description |
|---|---|---|
| `endpoint` | yes | `host:port` of the parent coordinator to connect to |
| `tls` | no | TLS settings for the upstream connection. See [`mode.proxy.tls` fields](#modeproxytls-fields) |
| `headers` | no | Map of static metadata headers attached to the outbound stream (e.g. bearer tokens, tenant IDs). Keys must be lowercase ASCII; the `grpc-` prefix is reserved |
| `compression` | no | `gzip` or empty (default) |
| `keepalive` | no | Client keepalive params. See [`mode.proxy.keepalive` fields](#modeproxykeepalive-fields) |

### `mode.proxy.tls` fields

| Key | Description |
|---|---|
| `ca_file` | Path to CA certificate PEM. If unset, system roots are used |
| `cert_file` | Client certificate PEM. Required for mTLS; `key_file` must also be set |
| `key_file` | Client private key PEM. Required when `cert_file` is set |
| `server_name_override` | Override `tls.Config.ServerName` (useful for IP-addressed endpoints with cert SAN mismatch) |
| `insecure_skip_verify` | Skip server certificate verification |

### `mode.proxy.keepalive` fields

| Key | Description |
|---|---|
| `time` | Send a keepalive ping after this much idle time (default: gRPC default — no pings) |
| `timeout` | Wait this long for ping ack before closing the connection. Must be less than `time` |
| `permit_without_stream` | Send pings even when there are no active streams |

## Metrics

Exposed at `metrics_listen/metrics` when `metrics_listen` is set.

| Metric | Type | Description |
|---|---|---|
| `coordinator_grpc_bytes_received_total` | counter | Bytes received from processors via gRPC |
| `coordinator_grpc_bytes_sent_total` | counter | Bytes sent to processors via gRPC |
| `coordinator_interesting_traces_total` | counter | Unique interesting traces seen by this coordinator |
| `coordinator_broadcast_drops_total` | counter | Keep decisions dropped because a processor's send buffer was full |
| `coordinator_broadcast_send_errors_total` | counter | Errors returned by `stream.Send` when broadcasting keep decisions |

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

**Proxy mode:** run multiple local coordinator instances per cluster, all pointing at the same parent endpoint. Each instance independently maintains its connection to the parent coordinator and broadcasts to its own connected processors. The parent coordinator deduplicates across all of them.
