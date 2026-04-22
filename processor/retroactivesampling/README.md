# retroactive_sampling processor

Tail-based sampling processor for OpenTelemetry Collector. Buffers spans on disk per collector host; keeps a trace if it matches any sampling rule. Propagates keep decisions to peer collectors via a coordinator service.

## How it works

1. Incoming spans are grouped by trace ID and written to the configured buffer file.
2. Each incoming batch of spans is evaluated immediately against the configured rules.
3. If interesting: delete from buffer → forward to next pipeline component → notify coordinator.
4. If not interesting: keep in buffer; evicted when `max_buffer_bytes` is exceeded (oldest first).
5. The coordinator broadcasts keep decisions received from any collector to all connected processors, which ingest matching buffered traces immediately.

## Configuration

```yaml
processors:
  retroactive_sampling:
    buffer_file: /var/otelcol/retrosampling.ring
    max_buffer_bytes: 1073741824  # 1 GiB
    max_interest_cache_entries: 100000  # optional, default shown
    coordinator_endpoint: coordinator:9090
    rules:
      - type: error_status
      - type: high_latency
        threshold: 5s
```

| Key | Required | Default | Description |
|---|---|---|---|
| `buffer_file` | yes | — | Path to the ring buffer file |
| `max_buffer_bytes` | yes | — | Max bytes of buffer disk usage; oldest traces evicted first |
| `max_interest_cache_entries` | no | `100000` | Max number of interesting trace IDs cached in memory for fast-path routing |
| `coordinator_endpoint` | yes | — | `host:port` of coordinator gRPC server |
| `rules` | yes | — | List of sampling rules (evaluated with OR logic) |

## Rules

| Type | Parameters | Keeps trace if… |
|---|---|---|
| `error_status` | — | any span has `StatusCodeError` |
| `high_latency` | `threshold: <duration>` | any span duration ≥ threshold |

Rules are evaluated with OR logic — first match wins.

## Including in a custom collector

Add to your `build.yaml`:

```yaml
processors:
  - import: pitr.ca/retroactivesampling/processor/retroactivesampling
    gomod: pitr.ca/retroactivesampling/processor/retroactivesampling v<version>
```

Then build:

```bash
builder --config build.yaml
```

Install the builder if needed:

```bash
go install go.opentelemetry.io/collector/cmd/builder@latest
```
