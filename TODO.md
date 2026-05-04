# TODO.md

## Small items

## Large items

- processor: the use of go.opentelemetry.io/collector/processor/processorhelper results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes), fix it but keep metrics we have now
- coordinator: support HTTP in addition to GRPC, processor and proxy coordinator should be able to choose through configs
