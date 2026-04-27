# TODO.md

## Small items

- coordinator: config should support env variables.

## Large items

- coordinator: support HTTP in addition to GRPC, processor should be able to choose through configs.
- processor: the use of go.opentelemetry.io/collector/processor/processorhelper results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes). Fix it
