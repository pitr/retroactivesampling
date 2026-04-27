# retroactive_sampling processor

Tail-based sampling processor for OpenTelemetry Collector. Buffers spans on disk per collector host; keeps a trace if it matches any sampling policy. Propagates keep decisions to peer collectors via a coordinator service.

The `policies:` configuration is compatible with [tailsamplingprocessor](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor) — policies that work on partial spans can be copied as-is. Exception: the `probabilistic` policy uses `hash_seed` (uint32) instead of `hash_salt` (string), matching [probabilisticsamplerprocessor](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/probabilisticsamplerprocessor) hashing.

## How it works

1. Incoming spans are grouped by trace ID and written to the configured buffer file.
2. Each incoming batch of spans is evaluated immediately against the configured policies.
3. If interesting: delete from buffer → forward to next pipeline component → notify coordinator.
4. If not interesting: keep in buffer; evicted when `max_buffer_bytes` is exceeded (oldest first).
5. The coordinator broadcasts keep decisions received from any collector to all connected processors, which ingest matching buffered traces immediately.

Some policies are deterministic across collectors — all collectors with the same config make the same decision, so spans are ingested locally without coordinator broadcast:
- `always_sample`: trivially, all collectors always sample.
- `probabilistic`: deterministic by trace ID hash — same seed + same trace ID = same decision.

## Configuration

```yaml
processors:
  retroactive_sampling:
    buffer_file: /var/otelcol/retrosampling.ring
    max_buffer_bytes: 1073741824  # 1 GiB
    max_interest_cache_entries: 100000  # optional, default shown
    coordinator_endpoint: coordinator:9090
    policies:
      - name: errors
        type: status_code
        status_code:
          status_codes: [ERROR]
      - name: slow
        type: latency
        latency:
          threshold_ms: 5000
      - name: probabilistic-10pct
        type: probabilistic
        probabilistic:
          sampling_percentage: 10
          hash_seed: 42  # optional; all collectors in the fleet must use the same value
```

| Key | Required | Default | Description |
|---|---|---|---|
| `buffer_file` | yes | — | Path to the ring buffer file |
| `max_buffer_bytes` | yes | — | Max bytes of buffer disk usage; oldest traces evicted first |
| `max_interest_cache_entries` | no | `100000` | Max interesting trace IDs cached in memory for fast-path routing |
| `coordinator_endpoint` | yes | — | `host:port` of coordinator gRPC server |
| `policies` | yes | — | List of sampling policies (evaluated with OR logic; first match wins) |

### Tuning `max_buffer_bytes`

Controls how long unsampled traces remain available for retroactive ingestion. Size it based on your per-collector pre-sampling span ingestion rate (all spans, before any sampling decision) and the window you want:

```
max_buffer_bytes = pre_sampling_ingestion_rate_bytes/s × desired_retention_seconds
```

Example: 50 MB/s pre-sampling ingestion rate, 30-second window → `1_500_000_000` (≈ 1.5 GiB).

A larger buffer increases the chance that a coordinator keep decision arrives before the relevant spans are evicted, at the cost of more disk usage. Traces are evicted oldest-first when the limit is reached. Provision at least this much free disk space on each collector host.

### Tuning `max_interest_cache_entries`

This is an in-memory cache of trace IDs for which a keep decision has already been received. When a span arrives for a cached trace ID it is forwarded immediately without a buffer lookup. The default (`100000`) covers most deployments. Increase it only if you observe cache eviction causing repeated buffer lookups under high interesting-trace throughput.

## Policies

Policies are evaluated in order with OR logic — first match wins.

### Supported policies

- `always_sample`: Sample all traces. Deterministic — skips coordinator broadcast.
- `latency`: Sample based on trace duration (earliest start to latest end). `threshold_ms` sets the lower bound; `upper_threshold_ms` sets the upper bound (omit for no upper bound).
- `status_code`: Sample based on span status code (`OK`, `ERROR`, `UNSET`).
- `string_attribute`: Sample based on string attributes (resource and span), with exact or regex matching.
- `numeric_attribute`: Sample based on numeric attributes (resource and span) by `min_value` and/or `max_value`.
- `boolean_attribute`: Sample based on boolean attributes (resource and span).
- `probabilistic`: Sample a percentage of traces by hashing the trace ID. Deterministic — all collectors with the same config make the same decision. Uses `hash_seed` (uint32) and 32-bit FNV, compatible with `probabilisticsamplerprocessor` (hash_seed mode).
- `trace_state`: Sample based on [TraceState](https://www.w3.org/TR/trace-context/#tracestate-header) key/value matches.
- `trace_flags`: Sample if the [sampled trace flag](https://www.w3.org/TR/trace-context-2/#sampled-flag) was set on any span.
- `ottl_condition`: Sample based on OTTL boolean expressions on spans or span events.
- `and`: Sample only if all sub-policies match.
- `not`: Sample based on the inverse of a single sub-policy.
- `drop`: Explicitly drop traces matching all sub-policies (halts the policy chain; overrides any later `sampled` result).

### Not supported

The following policies require complete trace data (all spans from all collectors) and are **not supported**:

- `span_count`
- `rate_limiting`
- `bytes_limiting`
- `composite`

### Examples

```yaml
policies:
  - name: errors
    type: status_code
    status_code:
      status_codes: [ERROR]

  - name: slow
    type: latency
    latency:
      threshold_ms: 5000
      upper_threshold_ms: 60000

  - name: probabilistic-10pct
    type: probabilistic
    probabilistic:
      sampling_percentage: 10

  - name: prod-errors
    type: and
    and:
      and_sub_policy:
        - name: env-prod
          type: string_attribute
          string_attribute:
            key: deployment.environment
            values: [production]
        - name: has-error
          type: status_code
          status_code:
            status_codes: [ERROR]

  - name: not-healthcheck
    type: not
    not:
      not_sub_policy:
        name: healthcheck
        type: string_attribute
        string_attribute:
          key: http.route
          values: [/health, /ready]

  - name: drop-bots
    type: drop
    drop:
      drop_sub_policy:
        - name: bot-attr
          type: boolean_attribute
          boolean_attribute:
            key: is_bot
            value: true

  - name: ottl-example
    type: ottl_condition
    ottl_condition:
      error_mode: ignore
      span:
        - 'attributes["env"] == "prod"'
      spanevent:
        - 'name != "ignored_event"'
```

## Platform

Linux and macOS only. Windows is not supported.

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
