# TODO.md

## Small items

- [x] probabilistic policy hashing should work the same way as https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/probabilisticsamplerprocessor
- [x] check for inefficient zap log attributes (like stringification)
- [x] drop decision is not handled by most composite policies or helpers. Check if this should be handled, maybe error out if it does not make sense in some situations.
- [x] check if any other processor policies should use sample local decision. Assume all collectors have the same policy config. Update processor README.
- [ ] add guidance to README on how to choose `max_buffer_bytes` and `max_interest_cache_entries`. For bytes, mention that they can use bytes ingested per collector to come up with a desired duration for how long traces are kept in buffer, thus impacting full trace ingestion. For entries, probably just mention that the default should be sufficient.
- [ ] tail sampling processor keeps component.Host for some reason, see if our processor needs it also.
- [ ] update coordinator readme with performance info. Say that the traffic it receives is on multiple orders lower than span traffic.

## Large items

- [x] import sampling policies (ie. evaluation) code from https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor so our processor can be a drop in replacement for it. Only support sampling policies that work on partial spans, so span_count, rate_limiting and bytes_limiting (maybe others) are not supported.
- [ ] evaluate coordinator design against high load (10k collectors, 10k interesting traces per second)
- [ ] re-analyze architecture of processor and coordinator. What are downsides of this approach vs tail sampling processor? How can they be fixed or mitigated?
- [ ] coordinator_endpoint string config in processor should instead be go.opentelemetry.io/collector/config/configgrpc
- [ ] include tail sampling processor in examples/ so that they can be compared by a manual test. Crete a separate otelcol config that does single collector tail sampling, use the same policies as the other configs. All ports should be different.
