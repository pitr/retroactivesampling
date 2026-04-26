# TODO.md

## Small items

## Large items

- processor: buffer keeps entries in a map. we need a better datastructure to keep allocations to a minimum. We also want to avoid reads when sweeping
- coordinator: in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
- coordinator: support HTTP in addition to GRPC, processor should be able to choose through configs.
- processor: the use of go.opentelemetry.io/collector/processor/processorhelper results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes). Fix it
