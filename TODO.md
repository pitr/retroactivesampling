# TODO.md

## Small items

- coordinator: config parsing should support env variables with dollar symbol
- buffer or processor: prevent multiple copies of processor using the same file

## Large items

- coordinator: support HTTP in addition to GRPC, processor should be able to choose through configs.
- processor: the use of go.opentelemetry.io/collector/processor/processorhelper results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes). Fix it
