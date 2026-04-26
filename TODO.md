# TODO.md

## Small items

- coordinator: server.go batches incoming requests until batch is full. Instead it should not wait forever
- coordinator: proxy pubsub should use context for recv/send to stop running, not recvErr

## Large items

- coordinator: in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
- coordinator: support HTTP in addition to GRPC, processor should be able to choose through configs.
- processor: the use of go.opentelemetry.io/collector/processor/processorhelper results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes). Fix it
