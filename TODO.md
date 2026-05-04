# TODO.md

## Small items

- buffer or processor: prevent multiple copies of processor using the same buffer file

## Large items

- coordinator: support HTTP in addition to GRPC, processor and proxy coordinator should be able to choose through configs
- processor: the use of go.opentelemetry.io/collector/processor/processorhelper results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes), fix it but keep metrics we have now
