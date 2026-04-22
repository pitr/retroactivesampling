# TODO.md

## Small items

- [x] probabilistic policy hashing should work the same way as https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/probabilisticsamplerprocessor
- [x] check for inefficient zap log attributes (like stringification)
- [ ] drop decision is not handled by most composite policies or helpers. Check if this should be handled, maybe error out if it does not make sense in some situations.
- [ ] check if any other processor policies should use sample local decision. Assume all collectors have the same policy config. Update processor README.

## Large items

- [x] import sampling policies (ie. evaluation) code from https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor so our processor can be a drop in replacement for it. Only support sampling policies that work on partial spans, so span_count, rate_limiting and bytes_limiting (maybe others) are not supported.
- [ ] evaluate coordinator design against high load (10k collectors, 10k interesting traces per second)
- [ ] re-analyze architecture of processor and coordinator. What are downsides of this approach vs tail sampling processor? How can they be fixed or mitigated?
- [ ] coordinator_endpoint string config in processor should instead be go.opentelemetry.io/collector/config/configgrpc
