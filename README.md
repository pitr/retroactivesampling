# Retroactive Sampling

Tail-based sampling for OpenTelemetry without a central aggregator.

Spans are buffered on disk per collector host. A trace is kept if any collector sees it as interesting (error status, high latency). Coordinators propagate that decision to all other collectors via Redis pub/sub.

Inspired by [KubeCon presentation from VictoriaMetrics folks](https://www.youtube.com/watch?v=ehKzt_kGEeM)

## Architecture

```
  otelcol + processor ─┐
  otelcol + processor ─┼──► coordinator ──► Redis
  otelcol + processor ─┘
```

Each collector runs the `retroactive_sampling` processor, which buffers spans on disk and evaluates sampling rules locally. When any span marks a trace as interesting, the processor notifies the coordinator. The coordinator deduplicates with Redis and broadcasts the keep decision to all connected processors, which emit their buffered spans for that trace.

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
