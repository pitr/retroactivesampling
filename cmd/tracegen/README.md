# tracegen

Synthetic trace generator and sampling validator for retroactive (tail-based) sampling pipelines.

## What it does

Emits synthetic OTLP traces to one or more collectors, then receives the sampled spans back and measures whether the sampling pipeline correctly identified and forwarded complete error traces.

**Dual role:**
- **Producer** — generates traces at a configurable rate, distributing spans across N simulated services. 1% of traces are "error" traces, each with one randomly chosen service span having error status. The rest are non-error (and should not be sampled).
- **Validator** — listens for spans forwarded back by the collectors. Tracks, per error trace, which of its N service spans were received (via a per-trace bitmask). After `wait` seconds, classifies each trace as `full` (all spans received), `partial` (some), or `none` (not sampled at all).

The trace ID encodes a session cookie, a monotonic counter, and an `isErr` flag directly in the 16-byte OTel trace ID. This lets the validator distinguish its own spans from foreign traffic and skip non-error traces without any state lookup.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-dst` | `localhost:4317,localhost:4318` | Comma-separated OTLP gRPC destinations (the collectors) |
| `-listen` | `0.0.0.0:9000` | OTLP gRPC endpoint tracegen listens on to receive sampled spans back |
| `-metrics` | `0.0.0.0:2112` | Prometheus metrics HTTP endpoint |
| `-rate` | `10` | Traces per second to emit |
| `-services` | `10` | Number of services (spans) per trace; max 64 |
| `-wait` | `20` | Seconds to wait for a trace's spans before classifying it |
| `-run` | `0` | Run duration in seconds, useful for e2e tests; 0 = run until interrupted |

## Metrics

| Metric | Labels | Description |
|--------|--------|-------------|
| `tracegen_traces_received_total` | `completeness={full,partial,none}` | Sampling quality — full means all spans of an error trace were forwarded |
| `tracegen_spans_received_total` | `reason={expected,non-error,foreign,corrupted,dup,late}` | Span classification; `non-error` should be zero (false positives) |
| `tracegen_traces_sent_total` | `error={true,false}` | Traces emitted |
| `tracegen_bytes_total` | `direction={in,out}` | Wire bytes |

## Output

When stopped via `-run N`, after waiting for in-flight spans tracegen prints all `tracegen_*` metrics to stdout in Prometheus text format. This is the sampling quality report.

## Usage example

```
# Run for 60 seconds, send to two collectors, receive sampled spans on :9000
tracegen -dst localhost:4317,localhost:4318 -listen 0.0.0.0:9000 -run 60
```

The collectors must be configured to forward sampled error traces back to `localhost:9000` (or whatever `-listen` is set to). The coordinator must be running on `localhost:9090` for cross-collector coordination to work.
