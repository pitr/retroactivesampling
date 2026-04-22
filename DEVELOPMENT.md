# Development

## Prerequisites

| Tool | Install |
|---|---|
| Go | https://go.dev/dl/ |
| protoc | `brew install protobuf` (macOS) or https://grpc.io/docs/protoc-installation/ |
| protoc-gen-go | `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` |
| protoc-gen-go-grpc | `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest` |
| otelcol builder | `go install go.opentelemetry.io/collector/cmd/builder@v0.150.0` |
| golangci-lint | https://golangci-lint.run/usage/install/ |
| mdatagen | `go install go.opentelemetry.io/collector/cmd/mdatagen@v0.150.0` |
| Redis | integration tests only — `brew install redis` (macOS) or https://redis.io/docs/getting-started/ |

## Make Targets

| Target | Description |
|---|---|
| `make build` | Build coordinator and tracegen into `bin/` |
| `make collector` | Build the example collector binary `bin/otelcol-retrosampling` using `example/build.yaml` |
| `make test` | Run unit tests across all modules |
| `make test-integration` | Run integration tests (requires `redis-server` on PATH) |
| `make proto` | Regenerate gRPC Go code from `proto/coordinator.proto` |
| `make generate` | Regenerate metadata from `processor/retroactivesampling/metadata.yaml` |
| `make lint` | Run golangci-lint across all modules |
| `make clean` | Remove `bin/` and `data/` |

## Example Folder

`example/` contains configs for a local two-collector setup that demonstrates cross-collector sampling.

| File | Purpose |
|---|---|
| `coordinator.yaml` | Coordinator — Redis on localhost, gRPC on :9090, Prometheus metrics on :9091 |
| `otelcol1.yaml` | First collector — OTLP gRPC on :4317, pprof on :1777, Prometheus on :8888 |
| `otelcol2.yaml` | Second collector — OTLP gRPC on :4318, pprof on :1778, Prometheus on :8889 |
| `build.yaml` | otelcol builder manifest for `bin/otelcol-retrosampling` |

To run the full local setup:

```bash
# terminal 1 — Redis
redis-server

# terminal 2 — coordinator
make build && bin/coordinator --config example/coordinator.yaml

# terminal 3 — first collector
make collector && bin/otelcol-retrosampling --config example/otelcol1.yaml

# terminal 4 — second collector
bin/otelcol-retrosampling --config example/otelcol2.yaml
```

Send traces to either collector with `bin/tracegen` (see `cmd/tracegen/`).
