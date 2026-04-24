# Retroactive Sampling

Tail-based sampling for OpenTelemetry without a central span aggregator.

Spans are buffered on disk per collector host. A trace is kept if any collector sees it as interesting (error status, high latency). Coordinators propagate that decision to all other collectors via Redis pub/sub.

Inspired by [KubeCon presentation from VictoriaMetrics folks](https://www.youtube.com/watch?v=ehKzt_kGEeM)

## Architecture

```
  otelcol + processor ─┐
  otelcol + processor ─┼──► coordinator ──► Redis
  otelcol + processor ─┘
```

Each collector runs the `retroactive_sampling` processor, which buffers spans on disk and evaluates sampling rules locally. When any span marks a trace as interesting, the processor notifies the coordinator. The coordinator deduplicates with Redis and broadcasts the keep decision to all connected processors, which emit their buffered spans for that trace.

## When to use this over tail sampling

Traditional tail sampling (e.g. the `tailsampling` OTel Collector processor) routes all spans to a central collector fleet for decision-making. This approach works, but has costs:

- **All span data must be forwarded** to the central fleet — every trace, sampled or not, travels an extra network hop.
- **A routing layer is required** (e.g. load-balancing exporter) to consistently hash traces to the same central collector.
- **Large centralized memory** — the central fleet must hold all in-flight traces in RAM for the full tail window.

Retroactive sampling avoids the forwarding hop entirely: spans stay on the collector that received them. Only a 20-byte trace ID is sent when a trace becomes interesting, and a small keep decision is broadcast back.

**Disk instead of memory:** buffered spans are written to an mmap'd file on each collector's local disk rather than held in RAM. The tail-window buffer cost is spread across the entire existing collector fleet instead of concentrated in a dedicated central cluster.

**Network break-even:** retroactive broadcast traffic equals traditional forwarding traffic at a collector count of:

```
trace_size_bytes / (20 × effective_sampling_rate)
```

For 15 KB average traces at 5% sampling that threshold is ~15,000 collectors. Below the threshold retroactive sampling uses less network; above it traditional wins on raw bytes, though the broadcast messages (20 bytes each) are far cheaper to process per byte than full span blobs, so the real compute break-even is higher still.

If your fleet approaches this threshold, consider switching to a [daisy-chain topology](coordinator/README.md#proxy) (proxy mode): each cluster runs a local coordinator that forwards to a central coordinator, reducing broadcast traffic from `I × P × M` to `I × K × M` where K is the number of clusters. This pushes the break-even up by a factor of P/K.

**Other advantages over tail sampling:**

- No central fleet or routing layer to operate.
- No consistent-hashing requirement — any collector can receive any span.
- Scales horizontally by adding coordinator instances.

See [PERFORMANCE.md](PERFORMANCE.md) for detailed traffic formulas and scaling analysis.

## Requirements

- Redis
- Coordinator service → [coordinator/README.md](coordinator/README.md)
- `retroactive_sampling` processor built into your collector → [processor/retroactivesampling/README.md](processor/retroactivesampling/README.md)

## Components

- **`coordinator/`** — standalone service: receives interesting-trace notifications via gRPC, deduplicates with Redis `SET NX`, broadcasts decisions to all connected processors. See [coordinator/README.md](coordinator/README.md).
- **`processor/retroactivesampling/`** — otelcol processor plugin. See [processor/retroactivesampling/README.md](processor/retroactivesampling/README.md).
- **`proto/`** — protobuf definitions for gRPC communication.
- **`example/`** — config examples for running a local two-collector setup. See [DEVELOPMENT.md](DEVELOPMENT.md).
- **`cmd/tracegen`** — load-testing tool.
