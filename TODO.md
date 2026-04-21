# TODO.md

## Small items

- [x] optimize proto/coordinator.proto - use bytes instead of string, remove useless `keep`
- [x] coordinator should try not to notify collector that gave it an interesting span — not worth it: 1/N sends saved (N=thousands), redundant onDecision is a mutex+map miss, essentially free
- [x] ensure "2 interesting spans in same trace" is handled properly without double writing or any other bugs
- [x] track as a metric the average time span lives on disk, based on data evicted in sweepOneLocked
- [x] replace buffer_dir in processor config with buffer_file or something, since we only ever need a single file
- [x] switch processor capability to MutatesData=false
- [ ] cmd/tracegen struggles to generate high rate traffic beyond `-rate=1000 -service=20`
- [x] migrate to range over `All()` when traversing telemetry data in processor
- [x] cmd/tracegen should shut down gracefully on ctrl-c
- [x] cmd/tracegen should print bytes out rate in a pretty way (kb/mb/gb if needed)
- [x] check if cache needs to do `lru.MoveToFront()`
- [x] add golangci-lint to makefile, fix any issues
- [ ] update readme with development section (what processes to run, how to install necessary tools like otelcol builder, how to run tests, etc)
- [ ] optimize groupByTrace in split.go
- [x] retroactive_sampling_buffer_span_age_on_eviction metric should have bucket_boundaries that able to catch ms values as well as up to 1 minute values. It should also probably be int, not float
- [ ] cleanup makefile. collector is missing dependencies. `proto` has no dependencies but should. `COLLECTOR_BIN` is unused. Every step should have all necessary dependencies
- [ ] coordinator should have graceful shut down (and not print `publish X: context canceled` forever)

## Large items

- [ ] import sampling (ie evaluation) code from https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor so our processor can be a drop in replacement for it
