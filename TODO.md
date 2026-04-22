# TODO.md

## Small items


## Large items

- [ ] evaluate coordinator design against high load (10k collectors, 10k interesting traces per second)
- [ ] re-analyze architecture of processor and coordinator. What are downsides of this approach vs tail sampling processor? How can they be fixed or mitigated?
- [ ] coordinator_endpoint string config in processor should instead be go.opentelemetry.io/collector/config/configgrpc
- [ ] include tail sampling processor in examples/ so that they can be compared by a manual test. Crete a separate otelcol config that does single collector tail sampling, use the same policies as the other configs. All ports should be different.
